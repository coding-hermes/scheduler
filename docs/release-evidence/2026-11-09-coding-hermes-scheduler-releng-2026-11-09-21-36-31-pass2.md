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

---

# Pass 3 — post-release sweep (third delivery of this tick)

Re-derived from scratch at the tip that existed when the lane fired a third time:
`682d1f1ad5af094e8f5c16fc3414392e4c90634f` (`chore(board): annotate QA-CHS-190 with the
cycle-3 act/setup-go datum`). Nothing from pass 2 is inherited except the facts re-proved below.

## Decision

**NO CUT.** `v1.16.0` is spent and stays exactly where it is; no new tag, no force-move, no
second release object. The range since the tag is bookkeeping only.

| Measure | Value |
|---|---|
| Tag | `v1.16.0` → tag object `9049119de442ea15043dc1b46fa891a2cbd49393`, peeled `28a676686e1f93db901c53b3e1408729574ea7e1` |
| `v1.16.0..HEAD` non-merge commits | 6 |
| `.go` files changed in range | **0** |
| `go.mod` / `go.sum` changed in range | **0** |
| Non-bookkeeping paths in range | **0** (filter excludes `.coding-hermes/`, `.gitreins/`, `docs/`, `CHANGELOG.md`) |
| Release tool verdict | `release-prep --check v1.17.0` → `REASON=VERSION_HEADING_EXISTS`; `--check v1.16.0` → `REASON=TAG_ALREADY_EXISTS` ("a version is spent once tagged") |

Range contents (all six):
`682d1f1a chore(board)`, `f611d9cb chore(board)`, `dff7f445 chore(board)`,
`38a0d261 chore(releng)`, `bf7df3ad qa(...)`, `50c6c6b3 gitreins: record ... lifecycle creation`.

## Published-artifact re-verification (independent of git)

Re-downloaded from the Release object, 1.5 h after publish, and compared byte-level:

| Check | Result |
|---|---|
| `SHA256SUMS` asset | 720 bytes, 8 entries (4 `schedulerd-*`, 4 `migrate-*`) |
| `schedulerd-linux-amd64` digest | `f08e1af45f0b6a68ee44dc2aad6bec7bfb88f39372ef80e16a4ddfebd15f5464` — **byte-identical** to the `SHA256SUMS` entry (no re-upload) |
| `go version -m` buildinfo | `mod github.com/coding-hermes/scheduler v1.16.0`; `vcs.revision=28a676686e1f93db901c53b3e1408729574ea7e1`; `vcs.time=2026-11-09T22:37:05Z`; `vcs.modified=false` |
| Toolchain stamp | `go1.26.10` — at or above the Go floor for the TLS trust-store fix (Go ≥ 1.26.10) |
| Version string in binary | present (5 occurrences of `v1.16.0`) |
| Release object | `isDraft=false`, `isPrerelease=false`, published `2026-11-09T22:57:35Z`, body 11,119 bytes (the in-tick notes fix survives) |

## CI truth (all ids observed live this pass, not quoted from pass 2)

| Surface | SHA / ref | Run | State |
|---|---|---|---|
| Exact tip | `682d1f1a` | `CI` **42725647462** | completed / success |
| Exact tip | `682d1f1a` | `CI Pipeline` **42725646256** | completed / success (polled 315 s) — 7/7 jobs: Lint, Race Detector (internal/api), Test (Go 1.26), Integration Test, Simulation Smoke Test, Build Binaries, CI All Clear |
| Tag | `v1.16.0` / `28a67668` | `Release` **42716162384** | completed / success (re-verified) |
| Tag | `v1.16.0` / `28a67668` | `CI Pipeline` **42716162309** | completed / success (re-verified) |
| Prior tips | `50c6c6b3`, `bf7df3ad`, `dff7f445` | — | CI + CI Pipeline completed / success |

No red run anywhere on the tip or the tag. The two `in_progress` Pipeline entries seen on
`f611d9cb` (`42724760843`) and on the tip at first probe were genuine queued work, not a stall:
`42725646256` reached `completed/success` on poll 7.

## Deployed-surface staleness (probe only — not claimed refreshed)

| Probe | Result |
|---|---|
| `GET 127.0.0.1:9090/api/v1/health` | HTTP 200 — stale |
| `build_sha` / `build_time` | `d0dad6d9` / `2026-11-07T05:42:56Z` (two minor versions behind `v1.16.0`) |
| `d0dad6d9..HEAD` | **169** commits, **20** of them touching Go source |
| `GET /healthz` | 200, HTML dashboard (that path is not the JSON health surface; the JSON one is `/api/v1/health`) |

Ownership unchanged: **SCHED-GAP-162** (stray listener / deployed-binary staleness) and
**RELEASE-051** (blocked on root-owned `/usr/local/bin/schedulerd` removal; owner consent needed).
This tick does not touch the live listener.

## Gates re-measured

| Gate | Result |
|---|---|
| Public posture | `private=false`, `visibility=public`, `license=MIT`, `default_branch=main` |
| Secret scanning | `secret_scanning=enabled`, `push_protection=enabled` |
| **`non_provider_patterns` / `validity_checks`** | **`disabled` / `disabled`** — `RELEASE-052` is still live, not a stale row (two PATCH-200-but-no-effect switches) |
| Branch protection | 1 approving review, code owners required, force-push **disabled**; `enforce_admins=false` (admins can bypass; recorded, not a finding) |
| `govulncheck ./...` | **No vulnerabilities found** (tree's `go.mod` is byte-identical to the tag's) |

## Dependency drift — measured, deliberately NOT filed as a new row

8 modules behind, every one transitive/toolchain-adjacent:
`golang.org/x/exp`, `x/mod 0.42.0→0.43.0`, `x/sync 0.24.0→0.25.0`, `x/sys 0.49.0→0.50.0`,
`x/text 0.43.0→0.44.0`, `modernc.org/cc/v4 4.31.0→4.34.0`, `ccgo/v4 4.38.1→4.39.0`,
`libc 1.80.1→1.80.2`. `govulncheck` reports no vulnerabilities, so this is routine semver churn,
not a security driver. `RELEASE-045-V` already established a bounded refresh cadence; filing an
identical row three days later would be board spam, so the census is recorded here and the row is
not created. If a future tick sees an advisory-backed bump, that changes the answer.

## Board

| Action | Row | Result |
|---|---|---|
| audit event | `RELEASE-054` (the sweep row that owns the cut) | one `audit` event at the new tip with these measurements — appended, status left `complete` |
| no create | — | `RELEASE-055` (guard file-scope) already open; dep drift deliberately unfiled |
| verified live | `RELEASE-051` (blocked), `RELEASE-052` (pending) | premises still true today, not stale |

## Watch item handed to the docs lane (not closed here)

`README-11` still reads "README stale vs last release **v1.12.0**: 359 commits since v1.12.0
shipped on main with no release". Its measured premise is now false — `v1.15.0`, `v1.15.1`,
`v1.15.2` and `v1.16.0` have all shipped — so the row needs re-basing, not closing. Not a releng
edit; recorded so the next docs/readme tick does not re-derive it from an obsolete number.

## Disclosures

1. **`GOCACHE` trap is live on this host.** `~/.cache/go-build` resolves through `/mnt/usb512`,
   which is *not mounted* in this pass, so any Go lane started without an explicit cache dies at
   cache init and reports a false RED. Every Go command in this pass ran with
   `GOCACHE=/var/tmp/hermes-scratch/gocache-releng116`. `GOMODCACHE=/home/kara/go/pkg/mod`
   (on-disk, fine).
2. **`govulncheck` ran in the canonical checkout, not the detached worktree.** `go.mod`/`go.sum`
   are unchanged in `v1.16.0..HEAD` (proved by the zero-count above), so the module graph it
   scanned is the tagged one; the scope is stated rather than implied.
3. **No binary from the Release was executed.** The earlier unattended-surface gate blocked
   executing downloaded artifacts and no consent has been obtained since, so integrity rests on
   `sha256sum` + `go version -m` + the workflow's own VCS-stamp gate — not on running the file.
4. All mutations happened in a detached worktree (`/tmp/releng-pass3.KyX3mk`) at the exact tip,
   never in the shared canonical checkout, which carried uncommitted sibling writes to
   `.coding-hermes/board/events.jsonl` at tick start.
