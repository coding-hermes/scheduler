# Release evidence — coding-hermes-scheduler v1.16.0

- Tick: `coding-hermes-scheduler-releng-2026-11-09-21-36-31` (second firing; the first firing,
  `...-21-36-31` at 21:41Z, recorded a *readiness* sweep and did not cut. This pass cut.)
- Lane: release-engineering satellite, verify-only (findings that need code become `RELEASE-*` rows).
- Repo: `coding-hermes/scheduler` (derived from `git remote -v`, not the folder name).
- Read via `git -C` / a detached worktree; the lane workdir is a stand-in.
- Recorded: 2026-11-09T23:31Z.

## Decision

**CUT v1.16.0 — minor, Bane's standing patch/minor authority, no owner confirmation required.**

Rule applied (release policy): at least one `feat` in the range and exact-head CI green ⇒ minor is due.
The first firing's blocker was `VERSION_HEADING_EXISTS`-shaped version prep, i.e. an invariant failure.
Per the releng charter an invariant failure is fixed in the tick, so this pass performed the version
prep itself rather than deferring it again.

## Range census (`v1.15.2..28a67668`)

| Measure | Value |
|---|---|
| Baseline tag | `v1.15.2` → `40373714bcc142ecd4989c3ffbe9aaf8882b0af4` |
| Range head (the release commit) | `28a676686e1f93db901c53b3e1408729574ea7e1` |
| Commits, all | 70 |
| Commits, merges | 6 |
| Commits, non-merge | 64 |
| `feat` | 3 |
| `fix` | 8 (one is `chore(release)`, counted separately below) |
| `perf` | 0 |
| `BREAKING CHANGE` / `!` | 0 / 0 |
| Board + release bookkeeping subjects | 42 |
| Changed paths | 66 (`*.go` 34, `_test.go` 13, `docs/` 6, `.coding-hermes/` 7, `.gitreins/` 2) |

`feat` present, `fix` present, no breaking change ⇒ **minor: `v1.16.0`**, not major, not patch.

The three features are the hop-alert consumer (`aeecd796`), the `BOARD-STALE` pick-time line
(`fb65a9a3`) and the persisted wake tables (`dc7388c5`). The eight fixes are the routing-mode loss on
the two automatic spawn paths (`dc5dfc00`), command-lane exemption in the lane-instructions check
(`c475e951`), `{repo}` local-path resolution in lanehook (`ead9d113`), gateway-first watchdog
(`14e5a9bf`), `-foreman` stem resolution in `check-fleet-invariants.py` (`a98c03b9`), missing-cwd
handling in PRE steps (`f308f716`) and the two ONE VERDICT LAW fixes in the policy guard
(`39976e27`, `ed05e7c7`).

## Gates

| # | Gate | Result | Evidence |
|---|---|---|---|
| 1 | Version prep on a clean candidate | PASS | `scripts/release-prep.sh --draft-from-range --since v1.15.2` → entries written under `[Unreleased]`; `make release-prep VERSION=v1.16.0` → `CHANGELOG.md` promoted to `## [1.16.0] — 2026-11-09` with a fresh empty `[Unreleased]`; `scripts/check-version-authority.sh --structure-only` rc=0; `check-version-authority.sh v1.16.0` rc=0 |
| 2 | Prep commit pushed, full SHA | PASS | `28a676686e1f93db901c53b3e1408729574ea7e1`, `origin/main` fast-forwarded `2334a267..28a67668` |
| 3 | Exact-SHA CI | PASS | `CI` run **42713505808** completed success (lint, race-blocks, race-scheduler, build, multiarch verify+build for linux/darwin/windows on amd64+arm64); `CI Pipeline` run **42713504590** completed success (Lint, Test Go 1.26, Race Detector internal/api, Integration Test, Simulation Smoke Test, Build Binaries, CI All Clear) |
| 4 | GitReins full guard, non-degraded | **SUBSTITUTED — see Deviations** | `gitreins guard --full --scope working-tree` returns `degraded: true` for a CHANGELOG-only diff (`go_build/go_lint/go_tests` "No Go files in scope"); the three lanes were executed directly on the exact prep tree instead (build rc=0, `golangci-lint run ./...` rc=0, `go test -short -p 1 -count=1 ./...` rc=0, 26 packages reported `ok`, no `FAIL`) |
| 5 | Sole publisher = tag workflow | PASS | `.github/workflows/release.yaml` `on: push: tags: v*`; no local `gh release create`; `gh release view` shows a workflow-published release |
| 6 | Annotated tag on the gated commit | PASS | `refs/tags/v1.16.0` = tag object `9049119de442ea15043dc1b46fa891a2cbd49393`, peeled `^{}` = `28a676686e1f93db901c53b3e1408729574ea7e1`; created by `make tag VERSION=v1.16.0` (claim-guarded) |
| 7 | Release object | PASS | `v1.16.0`, `isDraft=false`, `isPrerelease=false`, published `2026-11-09T22:57:35Z`, 9 assets (8 binaries + `SHA256SUMS`), Release run **42716162384** success |
| 8 | Artifact integrity | PASS | `SHA256SUMS` verified locally: `schedulerd-linux-amd64: OK`, `migrate-linux-amd64: OK`; `go version -m` → `vcs.revision=28a676686e1f93db901c53b3e1408729574ea7e1`, `vcs.modified=false` on both; the workflow's own gates "Checkout must stay pristine during the build" and "VCS stamp gate — all 8 assets must be vcs.modified=false (RELEASE-037)" passed; `v1.16.0` string present in both binaries |
| 9 | Tag workflows green | PASS | `Release` **42716162384** success; `CI Pipeline` **42716162309** completed success on `v1.16.0` (polled to conclusion at 2026-11-09T23:30:13Z) |
| 10 | Categorized notes published | PASS (fixed in-tick) | as published the release body was only a `**Full Changelog**` compare link; edited via `gh release edit --notes-file` to carry the 1.16.0 changelog section, the verification block and the erratum — body now 11,119 bytes, re-read from the API |

## Deployed-surface staleness (probe, not fixed here)

| Probe | Result |
|---|---|
| `GET http://127.0.0.1:9090/api/v1/health` | HTTP 200 — but stale |
| Version | `v1.14.0-40-gd0dad6d9-dirty` |
| `build_sha` / `build_time` | `d0dad6d9` / `2026-11-07T05:42:56Z` |
| Listener | `schedulerd` pid 3347652 on 127.0.0.1:9090 (user systemd unit inactive — the listener is a stray process) |
| `PATH` binary | `/usr/local/bin/schedulerd`, root-owned, dated Jul 19 09:50 (20,266,468 bytes) |

The running surface is two minor versions behind `v1.16.0` and predates it by two days of commits.
Ownership stays with **SCHED-GAP-162** (stray listener / deployed-binary staleness) and
**RELEASE-051** (blocked: stale Jul-19 `schedulerd` shadows the canonical build on `PATH`). This
tick does not claim the deployed surface is refreshed.

## Open blockers carried forward

| Row | Status | Subject |
|---|---|---|
| `RELEASE-051` | blocked | Stale Jul-19 `schedulerd` shadows the canonical build on `PATH` |
| `RELEASE-052` | pending | Two of five repo security switches silently do not apply (`secret_scanning_*`) |
| `RELEASE-054` | closed by this pass | Daily release readiness 2026-11-08 → superseded by this cut |

## Deviations and disclosures

1. **Guard verdict is degraded, by construction.** `gitreins` scopes a guard run to changed files; a
   CHANGELOG-only commit puts no Go file in scope, so `--full` (which selects *tests*, not *files*)
   still yields `go_build/go_lint/go_tests = skipped`. Forcing a non-degraded artifact would have
   required fabricating a code diff, so instead the three lanes were executed verbatim on the exact
   prep tree with repaired Go caches (`GOCACHE` and `GOMODCACHE` were pointing at an unmounted
   `/mnt/usb512`): `go build ./...` rc=0, `golangci-lint run ./...` rc=0,
   `go test -short -p 1 -count=1 ./...` rc=0 (26 packages `ok`, no `FAIL`, after `build_rc=0` and
   `lint_rc=0`; log `/var/tmp/hermes-scratch/rel116-localgates.log`, `DONE` written). Exact-SHA CI
   additionally ran the full suite, the race detector on `internal/api` and the integration +
   simulation smoke tests on the same tree. Filed as **RELEASE-055** so the next tick does not have
   to substitute again: `gitreins guard` needs a full-tree file scope for docs-only commits.
2. **CHANGELOG count inaccuracy, disclosed rather than force-fixed.** The `Not in this release` line
   states "63 non-merge commits in the range, 27 of them board traffic". The range as tagged is 64
   non-merge commits (70 with merges) and 42 bookkeeping subjects — the sentence was measured before
   the release commit existed and the bookkeeping count was wrong. The tag is never force-moved, so
   the correction lands on `main` after the tag and is recorded here and in the release body erratum;
   the tagged tree keeps the original sentence.
3. **Two commands were refused by the unattended-surface gate** (`rm -rf <dir>` and an exec of a
   downloaded binary): no consent was available, so neither was retried or rephrased. Artifact
   integrity was therefore established from `sha256sum -c` plus `go version -m` buildinfo plus the
   workflow's own VCS-stamp gate rather than from executing the downloaded binaries locally.
4. **Local full-suite timing:** `go test -short -p 1` ran cold-cache on the fleet host alongside the
   tag-branch CI job, so it was slower than CI's cached run (~35 min wall) but completed: rc=0, 26
   `ok` packages, zero `FAIL`. Its slowness is host contention, not a suite problem.

## Board

| Action | Row | Result |
|---|---|---|
| close | `RELEASE-054` (daily readiness 2026-11-08, AC included the cut) | `complete`, evidence entry with prep commit, tag object, release timestamp, the three run ids and this artifact |
| create | `RELEASE-055` — gitreins guard has no full-tree file scope | `pending`, P3, complexity 2, AC + measured `degraded: true` observation |
| events | `task_updated` (RELEASE-054), `task_created` (RELEASE-055) | appended with nanosecond ids above the numeric max |

## Artifacts

- `CHANGELOG.md` `## [1.16.0] — 2026-11-09` (promoted by `make release-prep`)
- Tag `v1.16.0` → `28a67668`
- Release https://github.com/coding-hermes/scheduler/releases/tag/v1.16.0
