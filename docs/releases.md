# Releases

How a release of the Coding Hermes Scheduler is cut, verified, and recorded.

This page exists because for the first five releases the procedure was implicit:
the only release tooling in the repo is `.github/workflows/release.yaml`, and it
was documented nowhere. A tag push *is* most of the procedure — but several
things that must be true before the tag are not enforced by anything, and one
release (v1.3.0) shipped through a path that never triggered the workflow at all.
Both are covered below.

**Audience:** the operator cutting the release. Everything here is a command you
can paste; where the expected output is quoted it was captured on 2026-09-20
from this repository, and the capture date is stated.

---

## 1. The actual mechanism

There is no release script that drives the release. The mechanism is GitHub
Actions plus one Makefile target that prepares the CHANGELOG:

| Piece | Path | Role |
|---|---|---|
| Release workflow | `.github/workflows/release.yaml` | Triggered by `push` with `tags: ['v*']`. Builds, smokes, checksums, and publishes. |
| CI (on tags too) | `.github/workflows/ci.yaml` | Also has `tags: ['v*']` in its `push` trigger, so a tag push runs the full CI Pipeline as well. |
| CI (main/PR only) | `.github/workflows/ci.yml` | `branches: [main]` on push and PR. **Does not run on tags.** |
| Version prep | `scripts/release-prep.sh` + `make release-prep` | Promotes the CHANGELOG `[Unreleased]` section. Does **not** tag or push. |

There is no `.goreleaser.yml`, no `Dockerfile`, and no `dist/` target. If you are
looking for one, it does not exist — do not invent it.

### What `release.yaml` does, step by step

1. **Trigger** — `on: push: tags: ['v*']`. Only a tag push. The workflow declares
   `permissions: contents: write` (needed to create the Release object).
   **It declares no `workflow_dispatch`** — there is no "Run workflow" button and
   no `gh workflow run release.yaml` path. Verified 2026-09-20:
   `grep -n 'workflow_dispatch' .github/workflows/release.yaml` → no match.
2. **Checkout** (`actions/checkout@v4`) and **Go toolchain** (`actions/setup-go@v5`,
   `go-version: '1.26'`).
3. **Build all platforms** — a nested `for os in linux darwin` / `for arch in amd64 arm64`
   loop, `CGO_ENABLED=0`, producing **8 binaries**:

   ```
   dist/schedulerd-linux-amd64   dist/schedulerd-linux-arm64
   dist/schedulerd-darwin-amd64  dist/schedulerd-darwin-arm64
   dist/migrate-linux-amd64      dist/migrate-linux-arm64
   dist/migrate-darwin-amd64     dist/migrate-darwin-arm64
   ```
4. **ldflags version injection** — `VERSION="${GITHUB_REF#refs/tags/}"` (i.e. the tag),
   plus `git rev-parse --short HEAD` and a UTC build date, injected as:

   ```
   -X github.com/coding-hermes/scheduler/internal/version.Version=${VERSION}
   -X github.com/coding-hermes/scheduler/internal/version.Commit=${COMMIT}
   -X github.com/coding-hermes/scheduler/internal/version.BuildDate=${BUILD_DATE}
   ```

   `internal/version.Current()` resolves ldflags → Go's native vcs buildinfo
   (`dev-<shorthash>`) → `dev`. The same identity serves `--version`,
   `/api/v1/health`, the OpenAPI `info.version`, and the MCP
   `serverInfo.version`, so there is exactly one version source to get right.
5. **Version smoke — an authority, not a formality.** After the build:

   ```sh
   ./dist/schedulerd-linux-amd64 --version | grep -q "schedulerd ${VERSION} " || { echo "version smoke FAILED"; exit 1; }
   ./dist/migrate-linux-amd64   --version | grep -q "migrate ${VERSION} "   || { echo "migrate version smoke FAILED"; exit 1; }
   ```

   If the built binary does not report the tag, **the release fails here**. This
   is the class-ender for the v1.1.0 defect, where the workflow injected
   `-X main.Version` while `main` declared no such variable — a silent linker
   no-op, so a v1.1.0 binary advertised a stale hardcoded `1.0.0` from three
   different surfaces (fixed by `3ef83638`, shipped as v1.1.1).
6. **Checksums** — `cd dist && sha256sum * > SHA256SUMS`.
7. **Release** — `softprops/action-gh-release@v2` with `files: dist/*` and
   `generate_release_notes: true`. That creates (or, if a Release object already
   exists for the tag, uploads into) the GitHub Release and generates notes from
   the commit range since the previous tag.

**The release workflow does not run the test suite.** It builds and smokes only.
Green CI on the commit you are tagging is the test gate — which is why it is a
precondition below, not an afterthought.

**Expected shape after a successful run:** 9 assets on the Release —
the 8 binaries plus `SHA256SUMS`.

---

## 2. Preconditions before tagging

Most of this list is enforced for you now; check each item in order. Where a
gate runs in CI it says so — the rest are still deliberate operator steps,
each with a command.

1. **Working tree clean** — `git status --porcelain` prints nothing.

   ```sh
   git status --porcelain    # expected: no output
   ```

2. **You are on the exact commit `origin/main` points at.**

   ```sh
   git fetch origin main
   git rev-parse HEAD origin/main   # expected: two identical SHAs
   ```

3. **Full gates green locally.** On a normal box:

   ```sh
   make test                    # = go test -short -count=1 ./...
   ```

   On the fleet host, run the sequential form (`-p 1`) — the host has cgroup
   pids limits and parallel packages are unsafe there:

   ```sh
   go test -short -p 1 -count=1 ./...    # expected: ok for every package
   ```

4. **CI green on the exact commit you are about to tag.** Not "CI was green
   earlier" — the run whose `headSha` equals your `HEAD`:

   ```sh
   gh run list --branch main --limit 5 \
     --json headSha,name,conclusion,createdAt \
     --jq '.[] | [.headSha[0:8], .name, .conclusion, .createdAt] | @tsv'
   ```

   The tag push also re-runs `CI Pipeline` (see §1), so a red commit gets caught
   twice: once by CI, once by you looking like the release broke it.

5. **The CHANGELOG names the version being tagged.** `make release-prep`
   produces this shape and release.yaml's **Version authority** step now
   enforces it at tag time (`scripts/check-version-authority.sh
   "${GITHUB_REF#refs/tags/}"`, above the build — see the rule below):

   ```sh
   make release-prep VERSION=v1.4.0
   git diff CHANGELOG.md
   ```

   Commit that change on `main` *before* tagging.

6. **No tag with that name already exists.**

   ```sh
   git rev-parse -q --verify refs/tags/v1.4.0 || echo "free"
   ```

### The version-authority rule (read this before tagging)

**A tag pushed over a tree whose CHANGELOG still says `[Unreleased]` now fails
in CI.** The gate landed with RELEASE-006 (2026-09-20): `release.yaml` runs
`scripts/check-version-authority.sh "${GITHUB_REF#refs/tags/}"` immediately
before the build — so a tag the CHANGELOG does not name fails fast and cheap,
never after a minutes-long multi-platform build:

```sh
scripts/check-version-authority.sh v1.4.0        # what the workflow step runs
# REASON=CHANGELOG_DOES_NOT_NAME_VERSION         # an unnamed tag, refused (exit 2)
```

The assertion itself is `release-prep.sh`'s `--check` mode — one
implementation of the rule, two callers — run by the wrapper against a
throwaway fixture copy of the CHANGELOG, so the operator-checkout
preconditions (`DIRTY_TREE`, `HEAD` == `origin/main`, tag-free) that a CI
checkout cannot satisfy are eliminated by construction rather than skipped.

The gate has deliberate limits, and they are worth naming:

- **It does not run on the out-of-band publish path.** §5's v1.3.0 precedent
  stands: a Release created through the API/CLI emits no `push` event, so
  *no* workflow — this gate included — runs. The gate closes the
  tag-pushed-over-an-unnamed-CHANGELOG hole, not the out-of-band one.
- **The binary version smoke (§1 step 5) stays.** The new step validates the
  TREE (the CHANGELOG describes the tag); the smoke validates the ARTIFACT
  (the binary reports the tag). They check different halves of version
  authority, and neither subsumes the other.
- **Ordinary pushes carry no version**, so CI (`ci.yaml`, on every push to
  main and every PR) asserts the weaker structural invariant instead —
  exactly one `[Unreleased]` heading and the newest release heading parsing
  as `## [X.Y.Z]` — through the same script's `--structure-only` mode:
  `scripts/check-version-authority.sh --structure-only`.
  `make release-prep` remains the operator step that produces the shape the
  gates demand.

Historical scope (before this gate landed): verified 2026-09-20, the grep
below genuinely returned no matches — the gap was real, and RELEASE-006 is
what closed it.

```sh
grep -rn 'CHANGELOG\|git describe' .github/    # before RELEASE-006: no matches; now: the two gate steps
```

### Preconditions `release-prep` enforces for you

`scripts/release-prep.sh` refuses (non-zero exit, named reason on stderr) rather
than guessing. It covers: repo present, tree clean, `HEAD` == `origin/main`,
`CHANGELOG.md` present, an `[Unreleased]` heading present, the requested version
heading not already present, the requested tag not already created, and a
non-empty `[Unreleased]` body. See §3.

---

## 3. The tag shape and the exact sequence

Tags in this repository are **annotated** tags — all five existing tags are tag
objects, not lightweight refs (`git cat-file -t v1.3.0` → `tag`). Keep it that
way: `git describe` and the release notes range depend on the tag object.

The full sequence, from a clean `main`:

```sh
# 0. start from the release commit
git switch main
git fetch origin && git reset --hard origin/main   # only if you are sure nothing local is wanted

# 1. roll the CHANGELOG over (writes CHANGELOG.md, nothing else)
make release-prep VERSION=v1.4.0
git diff CHANGELOG.md

# 2. commit the CHANGELOG rollover
git add CHANGELOG.md
git commit -m "chore(release): v1.4.0"

# 3. gates
make test

# 4. push the commit and let CI go green on it
git push origin main
gh run list --branch main --limit 3

# 5. tag and push the tag — this is the release action
git tag -a v1.4.0 -m 'Release v1.4.0'
git push origin v1.4.0
```

Step 5 is the entire release. `release-prep` deliberately does not run steps 2–5:
a command that tags and pushes when invoked is how a wrong tag gets published.

> Optional: `make tag` does exist and does exactly step 5, but only when you pass
> the version explicitly — `make tag VERSION=v1.4.0`. It is a separate, explicitly
> typed command, never something a prep run performs for you.

---

## 4. Post-tag verification

Run these immediately after the tag push. Commands marked ⟨ran⟩ were executed
while writing this page and their output is quoted as captured; the rest are the
same commands against a future tag.

### 4.1 The release workflow ran, on your tag

```sh
gh workflow list
```
⟨ran⟩ expected:
```
CI Pipeline	active	311964701
CI	active	311953863
Release	active	311988955
```

```sh
gh run list --workflow=release.yaml --limit 5
```
⟨ran⟩ expected shape (captured 2026-09-20, this is the full history — four runs,
one per tag that triggered the workflow):
```
completed	success	chore(gitreins): record MYPROJECT-GAP-049 approval	Release	v1.2.0	push	34782436078	1m43s	2026-09-13T20:59:32Z
completed	success	fix(scheduler): single-source build version — kill stale "1.0.0" hard…	Release	v1.1.1	push	33729089680	1m40s	2026-09-03T07:37:56Z
completed	success	fix: discard dead GetProject value in adaptive-cooldown re-pin test	Release	v1.1.0	push	33725384767	1m22s	2026-09-03T06:53:43Z
completed	success	chore: OPEN-001 — polish for v1.0.0 release	Release	v1.0.0	push	29653285119	1m31s	2026-07-18T17:06:50Z
```

The definitive per-tag trigger check — this is the one to trust, because it
counts runs whose `head_branch` is the tag itself:

```sh
gh api "repos/coding-hermes/scheduler/actions/runs?branch=v1.4.0&per_page=50" \
  --jq '.total_count'
```
⟨ran⟩ for the existing tags: `v1.0.0` → `2`, `v1.1.0` → `2`, `v1.1.1` → `2`,
`v1.2.0` → `2`, `v1.3.0` → **`0`**. Expect **2** for your tag (Release +
CI Pipeline, both `push`).

### 4.2 The Release object and its assets

```sh
gh release view v1.4.0
```
⟨ran⟩ for `v1.3.0` — expected shape, 9 assets:
```
title:	v1.3.0
tag:	v1.3.0
draft:	false
prerelease:	false
author:	totalwindupflightsystems
created:	2026-09-16T05:05:47Z
published:	2026-09-16T05:18:38Z
url:	https://github.com/coding-hermes/scheduler/releases/tag/v1.3.0
asset:	migrate-darwin-amd64
asset:	migrate-darwin-arm64
asset:	migrate-linux-amd64
asset:	migrate-linux-arm64
asset:	schedulerd-darwin-amd64
asset:	schedulerd-darwin-arm64
asset:	schedulerd-linux-amd64
asset:	schedulerd-linux-arm64
asset:	SHA256SUMS
```

**Asset count expectation: 9** (8 binaries + `SHA256SUMS`). Every published
release so far has had exactly 9:

```sh
for t in v1.0.0 v1.1.0 v1.1.1 v1.2.0 v1.3.0; do
  printf '%s ' "$t"; gh release view "$t" --json assets --jq '.assets | length'
done
```
⟨ran⟩: `v1.0.0 9`, `v1.1.0 9`, `v1.1.1 9`, `v1.2.0 9`, `v1.3.0 9`.

### 4.3 Re-run the version smoke against the *published* artifacts

The workflow's smoke runs against freshly built binaries in the runner's
workspace. Re-run it against the bytes that were actually published — that is
the strongest end-to-end confirmation available:

```sh
mkdir -p /tmp/release-check && cd /tmp/release-check
gh release download v1.3.0 \
  --repo coding-hermes/scheduler \
  --pattern 'schedulerd-linux-amd64' \
  --pattern 'migrate-linux-amd64' \
  --pattern 'SHA256SUMS' \
  --clobber
chmod 755 schedulerd-linux-amd64 migrate-linux-amd64

# only the three files we fetched are checked; the other 6 are reported missing
sha256sum --ignore-missing -c SHA256SUMS

VERSION=v1.3.0
./schedulerd-linux-amd64 --version | grep -q "schedulerd ${VERSION} " && echo "schedulerd version smoke PASS"
./migrate-linux-amd64   --version | grep -q "migrate ${VERSION} "   && echo "migrate version smoke PASS"
```
⟨ran⟩ 2026-09-20 against the published v1.3.0 assets — actual output:
```
schedulerd v1.3.0 (commit: 2c806da, built: 2026-09-16T05:16:58Z)
migrate v1.3.0 (commit: 2c806da, built: 2026-09-16T05:16:58Z)
schedulerd version smoke PASS
migrate version smoke PASS
migrate-linux-amd64: OK
schedulerd-linux-amd64: OK
```

`sha256sum -c` reports the six files you did not download as
`FAILED open or read` and then `WARNING: 6 listed files could not be read`. That
is expected for a partial download — pass `--ignore-missing` (as above) and read
the per-file `OK` lines.

---

## 5. When the workflow does not trigger

### The v1.3.0 precedent — read this before you assume it "just works"

v1.3.0 broke the pattern. Its tag exists on the remote and its Release object is
fully populated, but **zero workflow runs** happened for it:

```sh
gh api "repos/coding-hermes/scheduler/actions/runs?branch=v1.3.0&per_page=50" --jq '.total_count'
# 0
gh run list --limit 300 --json headBranch,workflowName \
  --jq '.[] | select(.headBranch=="v1.3.0") | .workflowName'
# no output — not Release, not CI Pipeline
```
⟨ran⟩ 2026-09-20. `release.yaml` is `active`, and `v1.0.0`/`v1.1.0`/`v1.1.1`/`v1.2.0`
each produced runs, so this is not a workflow that was switched off.

What the GitHub metadata says about how v1.3.0 was published differs from the
other four in exactly the ways an out-of-band (`gh release create` / Releases
API) publish would:

| tag | asset uploader | `targetCommitish` | release-workflow runs |
|---|---|---|---|
| v1.0.0 | `github-actions[bot]` | `main` | 1 |
| v1.1.0 | `github-actions[bot]` | `main` | 1 |
| v1.1.1 | `github-actions[bot]` | `main` | 1 |
| v1.2.0 | `github-actions[bot]` | `main` | 1 |
| **v1.3.0** | **`totalwindupflightsystems`** | **`2c806dac` (the commit SHA)** | **0** |

⟨ran⟩ 2026-09-20 via
`gh api repos/coding-hermes/scheduler/releases/tags/<tag> --jq '[.assets[]|.uploader.login]|unique'`
and `gh release view <tag> --json targetCommitish`.

**Inference, stated as an inference:** the v1.3.0 release was created through the
API/CLI rather than by pushing the tag from a checkout. A tag created by that
path does not emit a `push` event, so `on: push: tags` never fired. The
consequence is real and worth naming: **the version smoke did not run for
v1.3.0.** I verified the published artifacts by re-running it by hand (§4.3) and
both binaries report `v1.3.0` correctly, so no bad release shipped — but nothing
would have stopped one.

### What to do when no run appears

Work through these in order. There is no `workflow_dispatch` to reach for: the
workflow has only the `push: tags` trigger, and asking GitHub to dispatch it
fails with HTTP 422 — ⟨ran⟩ 2026-09-20:

```sh
gh workflow run release.yaml
```
```
could not create workflow dispatch event: HTTP 422: Workflow does not have 'workflow_dispatch' trigger (https://api.github.com/repos/coding-hermes/scheduler/actions/workflows/311988955/dispatches)
```

Do not treat that as a bug to fix mid-release.

1. **Confirm the tag object exists and points where you think.**

   ```sh
   git ls-remote --tags origin | grep v1.4.0
   git rev-parse v1.4.0^{commit}
   ```

   Annotated tags appear twice in `ls-remote` (the tag object and its `^{}`
   peeled commit). Note which SHA the tag points at.

2. **Check the Release object before reacting.** If assets are already present
   and correct (uploader is whoever published, `targetCommitish` is a SHA, count
   is 9), the release may be complete through the out-of-band path — v1.3.0
   is exactly this case. Verify it properly rather than assuming (§4.3), and
   record that the smoke was run by hand because the workflow did not run.

3. **If the release is incomplete, reproduce the workflow's outputs by hand.**
   This is the option that does not touch a published tag. Build with the same
   ldflags the workflow uses, run the same smoke, checksum, and upload:

   ```sh
   VERSION=v1.4.0
   COMMIT="$(git rev-parse --short HEAD)"
   BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
   LDFLAGS="-s -w -X github.com/coding-hermes/scheduler/internal/version.Version=${VERSION} -X github.com/coding-hermes/scheduler/internal/version.Commit=${COMMIT} -X github.com/coding-hermes/scheduler/internal/version.BuildDate=${BUILD_DATE}"
   mkdir -p dist
   for os in linux darwin; do
     for arch in amd64 arm64; do
       CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags="${LDFLAGS}" -o "dist/schedulerd-${os}-${arch}" ./cmd/schedulerd/
       CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags="${LDFLAGS}" -o "dist/migrate-${os}-${arch}"   ./cmd/migrate/
     done
   done
   ./dist/schedulerd-linux-amd64 --version | grep -q "schedulerd ${VERSION} " || { echo "version smoke FAILED"; exit 1; }
   ./dist/migrate-linux-amd64   --version | grep -q "migrate ${VERSION} "   || { echo "migrate version smoke FAILED"; exit 1; }

   (cd dist && sha256sum * > SHA256SUMS)
   gh release upload v1.4.0 dist/* --clobber
   ```

   Then verify as in §4.3. This is the honest fallback: the artifacts are real,
   built from the tagged commit, and the smoke ran on your machine.

4. **Re-fire the `push` event by re-pushing the tag — only if you accept the
   consequences.** This is the only way to make the *workflow* run again, and it
   is destructive-ish, so it is last and it is deliberate:

   ```sh
   git push origin --delete v1.4.0
   git tag -a v1.4.0 -m 'Release v1.4.0'        # re-create if needed
   git push origin v1.4.0
   ```

   Deleting and re-pushing a tag re-points a name other people may already have
   fetched. If the Release object already exists, `action-gh-release` will upload
   into it rather than create a second one. Prefer option 3 unless the workflow
   run itself (its logs, its provenance) is what you need.

5. **Do not "fix" it by pushing a `workflow_dispatch` trigger into
   `release.yaml` mid-release.** That changes the release surface to work around
   one incident. If you conclude the operator ergonomics genuinely need it, file
   it as a board row and land it as a normal reviewed change, then document the
   button here.

---

## 6. Semver policy

This repository is a daemon with a fleet-facing HTTP API (`/api/v1/*` — 28 paths
in the OpenAPI spec at the time of writing, pinned against `docs/api.md`'s route
table by `TestAPI_OpenAPI_Success`), an MCP surface at `/mcp`, and a CLI with a
documented flag table. Those three surfaces are the contract; `scheduler.db` is
durable operator state. Breaking any of them is what a major version is for.

> The path count grows with features (19 at v1.1.0's `GAP-057`, 28 now). Treat
> the number as a reading, not a contract — the contract is that the spec and
> `docs/api.md` agree, which the test above enforces.

### MAJOR — a consumer or an existing deployment must change something

- Removing or renaming an `/api/v1/*` route, an MCP tool, a CLI flag, or a
  documented config key (TOML / env var).
- Renaming a response field, changing its type, or changing the meaning of an
  enum value a consumer can observe (e.g. redefining a `ticks.status` value).
- A schema migration that cannot be applied automatically on daemon start, that
  loses data, or that makes an existing `scheduler.db` or `fleet.toml` fail to
  load. Migrations in this repo are forward-only and versioned
  (`internal/database/migrations.go`), so a migration that a running deployment
  cannot survive is a breaking change even if the code compiles.
- Docker/systemd/deployment contract changes an operator must act on
  (`deploy/coding-hermes-scheduler.service`, the gateway env-file contract).

### MINOR — additive and backward compatible

- New endpoints, new MCP tools, new CLI flags, new optional request fields, new
  response fields, new OpenAPI paths.
- New scheduling behaviour that respects documented contracts — a merged
  feature wave. Most releases here are this: v1.1.0 (adaptive cooldown, ordered
  model chains, namespace caps, blocks API), v1.2.0 (concurrent wave scheduling
  SCHED-GAP-108..115), v1.3.0 (the ADV-R04..R08 scheduler-advancement wave).
- A new automatic migration that upgrades an existing DB on start without
  operator action.
- Behaviour fixes that change *policy outcomes* an operator relies on
  (e.g. cooldown escalation semantics) — if a documented policy statement in
  `AGENTS.md` must be rewritten, lean minor even though nothing broke.

### PATCH — no contract change

- Crash/hang fixes, misclassification fixes, logging and observability fixes,
  docs, tests, dependency bumps, CI. **v1.1.1 is the precedent: exactly one
  commit (`3ef83638`), a fix, no features — patch.**

### Notes and precedent

- **Nothing has been a major yet.** All five releases are 1.x, and across the
  four ranges reconstructed in `CHANGELOG.md` there are **0 breaking-change
  commits** (`type(scope)!: ` subjects): verified with
  `git log --format='%s' <range> | grep -cE '^[a-z]+(\([^)]*\))?!: '`.
- **Pre-1.0 is in the past** — v1.0.0 shipped 2026-07-18. Do not reintroduce a
  0.x series.
- **When in doubt, go minor.** A minor that should have been a patch costs a
  reader a paragraph; a patch that should have been a minor silently breaks
  someone. But a re-release of the *same* content with one fix on top (the
  v1.1.0 → v1.1.1 shape) is a patch, not a new minor.
- **The version string is the tag, verbatim.** `release.yaml` derives it from
  `${GITHUB_REF#refs/tags/}`, so `v1.4.0` is what every binary, `/api/v1/health`,
  the OpenAPI spec, and MCP report. Do not put extra text in a tag.
- **One version, one CHANGELOG heading.** `make release-prep` refuses if the
  heading already exists, so a version cannot be released twice by accident.

---

## 7. Release history (tag → commit → range)

This is the evidence base for the reconstructed `CHANGELOG.md` entries. All five
tags are annotated tag objects and all resolve:

```sh
for t in v1.0.0 v1.1.0 v1.1.1 v1.2.0 v1.3.0; do
  printf '%s -> %s (%s)\n' "$t" "$(git rev-parse ${t}^{commit})" "$(git for-each-ref --format='%(creatordate:short)' refs/tags/$t)"
done
```

| tag | tagged commit | annotated | commits in range | CHANGELOG section |
|---|---|---|---|---|
| v1.0.0 | `0fa79e9a` | yes | — (initial) | `## [1.0.0] — 2026-07-18` |
| v1.1.0 | `ca991957` | yes | `v1.0.0..v1.1.0` = 1117 | `## [1.1.0] — 2026-09-03` |
| v1.1.1 | `3ef83638` | yes | `v1.1.0..v1.1.1` = 1 | `## [1.1.1] — 2026-09-03` |
| v1.2.0 | `b6f7d4ef` | yes | `v1.1.1..v1.2.0` = 167 | `## [1.2.0] — 2026-09-13` |
| v1.3.0 | `2c806dac` | yes | `v1.2.0..v1.3.0` = 36 | `## [1.3.0] — 2026-09-16` |

⟨ran⟩ 2026-09-20, in this worktree. Dates are the tag's `creatordate`
(`git for-each-ref --format='%(creatordate:short)' refs/tags`).

---

## 8. Keeping the CHANGELOG honest

`CHANGELOG.md` is the reviewable record of what shipped. GitHub Release notes are
generated by the workflow (`generate_release_notes: true`) and live on the
Release object — off-repo and not reviewable in a PR. Both exist; the CHANGELOG
is the one that gets reviewed, so it is the one that must be rolled over before
the tag.

The rule, in one line:

> **Tag only what the CHANGELOG already names.**

`make release-prep VERSION=vX.Y.Z` is what makes that true. It:

1. validates the version shape and every precondition in §2 (refusing by name),
2. promotes the current `[Unreleased]` body into `## [X.Y.Z] — <today>`,
3. leaves a fresh, empty `[Unreleased]` above it,
4. prints the next commands (gates, commit, tag, push) **without running them**.

Idempotency: running it a second time for the same version **refuses**
(`VERSION_HEADING_EXISTS`) rather than duplicating the heading — see the script's
`--help` and the refusal table below.

| refusal | reason code | what to do |
|---|---|---|
| no argument / several | `BAD_USAGE` | `make release-prep VERSION=v1.4.0` |
| `1.4.0`, `v1.4`, `v1.4.0-rc1` | `BAD_VERSION_SHAPE` | annotated-tag shape is `vMAJOR.MINOR.PATCH` |
| modified/untracked files | `DIRTY_TREE` | commit or clean them; the script lists them |
| `HEAD` != `origin/main` | `NOT_ON_RELEASE_COMMIT` | `git fetch origin main`, get onto that commit |
| no `[Unreleased]` heading | `MISSING_UNRELEASED` | the CHANGELOG was hand-edited into an unrecoverable shape |
| `## [1.4.0]` already present | `VERSION_HEADING_EXISTS` | a different version, or the rollover already happened |
| `v1.4.0` tag already exists | `TAG_ALREADY_EXISTS` | the version is spent |
| `[Unreleased]` has no content | `EMPTY_UNRELEASED` | write the entries first; a release heading with nothing under it is the thing this tool exists to prevent |
| `CHANGELOG.md` missing | `MISSING_CHANGELOG` | wrong directory |
| not a git repo | `NOT_A_REPO` | wrong directory |

**Scope of the reconstructed entries (2026-09-20).** `CHANGELOG.md` was frozen
from 2026-08-04 (`167cc99`) while four releases shipped. The entries for
v1.1.0/v1.1.1/v1.2.0/v1.3.0 were reconstructed from real commits and real tags:
each heading cites its tag and its commit range, and the `v1.0.0..v1.1.0` entry
(1117 commits, over 150) is summarised by category with the fleet's row ids
named rather than listing every commit. Nothing in those entries was invented —
every line traces to a commit subject or a range cited under its heading.
