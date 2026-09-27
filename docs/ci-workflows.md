# CI workflows

This repo has TWO active GitHub Actions CI workflows. Both gate every push to
`main` and every PR against `main`, so the README carries a badge for each —
a single badge would report only one workflow's result while both gate the
push.

## The two workflows

| | `.github/workflows/ci.yml` | `.github/workflows/ci.yaml` |
|---|---|---|
| Workflow name | `CI` | `CI Pipeline` |
| Triggers | push `main`, PR `main` | push `main` **+ tags `v*`**, PR `main` |
| Jobs | `build`, `multiarch`, `lint`, `race` | `lint`, `test` (matrix), `simulation`, `integration`, `build-artifacts`, `all-clear` |

### What only ci.yml runs

- Full (non-`-short`) test suite (`go test -count=1 ./...`, `build` job)
- Live-config invariant fixture battery (`scripts/check-invariants.sh`,
  SCHED-GAP-147, `build` job)
- Board closure-evidence gate (`schedulerd --verify-board` on a fixture board,
  SCHED-GAP-085, `build` job)
- End-to-end scheduler verify (`schedulerd --test-verify 3`)
- Dedicated race job on `internal/blocks/...` + `internal/scheduler/...` with
  a 900s timeout (CI-004)
- Org multi-arch build/attestation via the reusable
  `coding-hermes/.github` workflow (SCHED-GAP-159)

### What only ci.yaml runs

- Simulation smoke test (`-sim-setup -sim-ticks 3` with output assertions)
- Integration-tagged tests (`go test -run TestIntegration -tags integration`
  over the root `integration_test.go` — the only place that build tag is
  exercised; see also `docs/integration.md`)
- CHANGELOG structural hygiene (`scripts/check-version-authority.sh
  --structure-only`, RELEASE-006)
- Go matrix job with coverage artifact upload, plus an `all-clear` gate job
- Runs on `v*` tag pushes (the release path's CI companion)

### Shared (duplicated) ground

`go build ./...`, `go vet ./...`, `go test -count=1 -short ./...`, and
`golangci-lint` run in both. The race detector also overlaps: `ci.yaml` races
the whole tree (`go test -short -race`), while `ci.yml` races the two
race-sensitive packages with `-p 1` and a longer timeout.

## Canonical status: NEITHER — both are load-bearing

Neither file is a strict superset of the other. Each runs steps the other
does not (listed above), so retiring either would silently drop real gates:

- Retiring `ci.yml` loses the invariant battery, board gate, full test run,
  dedicated race lane, and multi-arch builds.
- Retiring `ci.yaml` loses the simulation smoke, integration-tagged tests,
  CHANGELOG hygiene, and tag-push CI.

The repo's own inventory agrees (`README.md`, repository layout section):
both are live, and merging them "is filed as a finding, not done blind."

Evidence that both gate every push — paired runs seconds apart for the same
commit (`gh run list --repo coding-hermes/scheduler --limit 12`):

    CI            | board: close DOC-5 ... | main | failure | 2026-09-27T20:15:05Z
    CI Pipeline   | board: close DOC-5 ... | main | success | 2026-09-27T20:15:04Z

Note the divergence above: a single `CI` badge would have shown red while
`CI Pipeline` was green for the same commit — the reason the README shows
both badges.

Follow-up (maintainer decision, outside docs scope): consolidate the two
files into one workflow so one badge is honest again, keeping every unique
step listed above. Until then, keep both badges.

## `[ci skip]`

Neither workflow documents a `[ci skip]` convention; no commit-message skip
token is honoured by these files (GitHub Actions honours `[ci skip]` /
`[skip ci]` natively in commit messages, but this repo's docs do not
establish a convention around it — do not rely on skipping CI for merges to
`main`).
