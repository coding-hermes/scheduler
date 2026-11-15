# Release readiness evidence — coding-hermes/scheduler

Tick: `coding-hermes-scheduler-releng-2026-11-14-04-57-03`

## Candidate

- Repository: `coding-hermes/scheduler`
- Canonical remote: `https://github.com/coding-hermes/scheduler.git`
- Default branch: `main`
- Code-certified candidate: `5a93f1ad2a573ff267eeb6d5fcd34b152ff793ac`
- Baseline: `v1.16.0` → `28a676686e1f93db901c53b3e1408729574ea7e1`
- `v1.17.0` tag and GitHub Release: absent
- The stand-in checkout is behind the fetched remote by 33 commits and ahead by one local-only commit; all measurements use the fetched remote candidate.

## Conventional-commit census

Range `v1.16.0..5a93f1ad2a573ff267eeb6d5fcd34b152ff793ac`:

- 168 total non-merge commits; class sum 168.
- `feat`: 2; `fix`: 8; `perf`: 7; other conventional: 150; non-conventional: 1.
- Breaking subjects/footers: 0.
- Proposal: MINOR `v1.17.0`. No major confirmation is required because no breaking marker exists.
- Seventeen `feat`/`fix`/`perf` subjects are present in the full range. The prepared `[1.17.0]` section does not cover the post-prep material commits; `RELEASE-060` remains open.
- Material subjects: `feat(scheduler): admission-time cheap skip for tasks-mode lanes with no actionable rows (SCHED-GAP-17007-M1)`; `feat(dispatch): scheduler-dispatch emits reply_to and text from config; correlation keys named (SCHED-GAP-16983-A)`; eight `fix` subjects and seven `perf` subjects, including duplicate/measurement-only subjects shown by the range census.

## Version and release tooling

- `scripts/release-prep.sh`, `scripts/check-version-authority.sh`, and the `make release-prep`/`make tag` targets are present.
- `scripts/check-version-authority.sh --repo <candidate-tree> v1.17.0` returned PASS; `[Unreleased]` occurs exactly once.
- The repository-owned path is `make release-prep` followed by `make tag`. `make tag` requires a clean tree and `HEAD == origin/main`, then uses `scripts/release-claim.sh` before creating and pushing an annotated tag.
- `.github/workflows/release.yaml` is tag-triggered and builds eight `CGO_ENABLED=0` static binaries plus `SHA256SUMS`.
- No Dockerfile or GoReleaser configuration exists.

## CI exact-head evidence

- Candidate `5a93f1ad2a573ff267eeb6d5fcd34b152ff793ac` has exact-SHA CI run `43680386864` (`CI`) completed/success and `43680386377` (`CI Pipeline`) completed/success. Per-job readback showed build, lint, invariant fixture battery, binary build, board closure-evidence, end-to-end verify, vet, short/full tests, race detector, integration, simulation smoke, and all cross-build verification jobs successful; tag-only Container and Release jobs were skipped by design on a branch push.
- Exact candidate check-runs total: 2 check suites, with all non-tag required checks successful. This gate is green for the candidate.
- Earlier failures in the recent history are on older SHAs (`8925975a`, among others) and do not transfer to this candidate.
- A separate board/evidence carrier has not been created in this pass; carrier CI is therefore not applicable.

## GitReins and dogfood

- No `.gitreins/history` verdict is bound to the candidate. A fresh `gitreins guard --full --scope working-tree --json` from the candidate worktree was not completed in the final probe because the worktree had already been removed; the earlier candidate-local full gates were independently run. The repository's recent release evidence identifies GitReins as degraded for docs/board-only scope, so candidate-bound non-degraded GitReins evidence remains UNVERIFIED.
- No candidate-exact dogfood/QA receipt names `5a93f1ad`. The latest dogfood record is the 2026-11-12 battery: install/upgrade/tests/resource/chaos-disconnect proven, lint and introspection cells INFO/UNVERIFIED, and CI-act affected by harness/toolchain issues. It is historical and not a waiver. Candidate-bound dogfood/QA evidence is UNVERIFIED.

## Security and deployment

- `go.mod` pins `toolchain go1.26.11`.
- Candidate-local `go build ./...` exited 0, `go vet ./...` exited 0, and `go test -short -p 1 -count=1 ./...` exited 0 across the repository packages.
- Candidate-local `govulncheck ./...` returned exit 0: `No vulnerabilities found.`
- Live `http://127.0.0.1:9090/api/v1/health` returned HTTP 200 with `db=connected`, `build_sha=9b1fdd22`, and version `v1.16.0-58-g9b1fdd22-dirty` for PID `276029`. The deployed daemon is stale versus the candidate and remains owned by `SCHED-GAP-162`/`RELEASE-051`. No deployment mutation was made.

## Known issues

- `RELEASE-057`: candidate-bound GitReins and dogfood/QA evidence remain unverified.
- `RELEASE-058`: toolchain vulnerability blocker is cleared on the candidate, but its board row remains pending and has not been read back as closed.
- `RELEASE-059`: the board-vocabulary correction is present in the candidate's recent history, but the row remains pending and its completion evidence is degraded/pending in the board record.
- `RELEASE-060`: the prepared `[1.17.0]` changelog does not cover all material commits in the full `v1.16.0..5a93f1ad` range.
- Deployed schedulerd remains stale at `9b1fdd22` / v1.16.0-derived dirty build.

## Decision

**NO CUT.** Do not tag, publish, deploy, or mutate the changelog. Exact candidate CI is green and local build/test/vulnerability gates pass, but candidate-bound GitReins/dogfood evidence is unverified, the changelog coverage row remains open, and the current board still carries pending release blockers. Existing tags and Releases were not modified.
