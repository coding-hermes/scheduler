# Release readiness sweep — coding-hermes/scheduler — 2026-11-07 (tick releng-2026-11-07-15-25-21)

## Target and identity

- Repository: `coding-hermes/scheduler` — remote `https://github.com/coding-hermes/scheduler.git` (derived from `git remote -v`, not from a folder name).
- Canonical checkout: `/home/kara/coding-hermes-scheduler/coding-herms-scheduler`.
- Default branch: `main`.
- Verified remote tip: `6ae3193cc58ec3c7de4c1d98677a7d4aba5c1fe8` (`git ls-remote origin refs/heads/main` agrees).
- All gates and probes below ran in a fresh **detached worktree** at that SHA (`/home/kara/scheduler-releng-wt-20261107-1715`, 0 dirty entries), because the canonical checkout was 3 commits behind and dirty with an in-flight DRY-RUN THROTTLE worker (`internal/scheduler/dry_run_throttle.go`, uncommitted).
- Latest release artifact: annotated tag `v1.14.1` → tag object `77f64d47…`, dereferences to `de573541d21d8402fedcf8f7396702066546c96e`.

## Decision 1 — is a release due? NOT DUE

Range `v1.14.1..origin/main` = **4 commits**, and the whole range touches **3 files**:

```
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
docs/release-evidence/2026-11-07-v1.14.1.md
```

Census by conventional prefix: 0 feat, 0 fix, 0 perf, 0 `BREAKING CHANGE`, 0 `type!` — every commit is `pm:` / `chore(releng):` bookkeeping (3 of the 4 carry `[ci skip]`). Zero material commits ⇒ rule "zero material commits → not due". The last release shipped 36 minutes before this tick (`2026-11-07T16:39:24Z`).

## CI truth on the tip

| SHA | run | workflow | state |
|---|---|---|---|
| `6ae3193c` (tip) | — | — | **no run by construction** (`[ci skip]`, board-only diff) |
| `517eca42` | 42392863396 | CI | **FAILURE** |
| `517eca42` | 42392862306 | CI Pipeline | success |
| `de573541` (release target) | 42389771108 / 42389770383 | CI / CI Pipeline | success / success |
| `de573541` (tag-triggered) | 42392333708 | Release | success (published 9 assets) |
| `de573541` (tag-triggered) | 42392333648 | CI Pipeline | cancelled (duplicate of 42389770383, disclosed) |

The nearest board/code-bearing run to the tip is **RED**: run `42392863396`, job `build`, step `Invariant fixture battery (SCHED-GAP-147)`:

```
VIOLATION board-vocab SCHED-GAP-128: status=blocked
VIOLATION board-vocab REL-GUARD-001: status=blocked
VIOLATION board-vocab SCHED-GAP-16937: status=blocked
FAIL — 3 violation(s)
BATTERY: FAIL (board-only; checker=1 lint=0 pytest=0)
```

Reproduced locally on the current tip (`6ae3193c`), which now carries a fourth parked row (`SCHED-GAP-16934`, added by the `[ci skip]` commit `6ae3193c`, so no run ever saw it):

```
VIOLATION board-vocab SCHED-GAP-128: status=blocked
VIOLATION board-vocab REL-GUARD-001: status=blocked
VIOLATION board-vocab SCHED-GAP-16934: status=blocked
VIOLATION board-vocab SCHED-GAP-16937: status=blocked
FAIL — 4 violation(s) (board-only mode: fleet checks 1-7 not run — no local DB/TOML)
```

Consumer impact: `ops/check-fleet-invariants.py` exits 1, so `scripts/check-invariants.sh` exits 1, so the `build` job fails on **every** push to `main` until resolved. No release can be cut while this stands (exact-SHA CI red).

## The gate defect: `status=blocked` is rejected by the writer gate but supported by the reader and the canonical writer

Three shipped surfaces disagree about one spelling:

| Surface | Position |
|---|---|
| `internal/scheduler/board_vocab.go` `BoardAllowedStatuses` | `pending, in_progress, complete, duplicate` — `blocked` **illegal** |
| `ops/check-fleet-invariants.py` `BOARD_ALLOWED_STATUSES` (line 372) | same four — `blocked` **illegal** (CI gate) |
| `internal/scheduler/board_freshness.go` `RowBlocked = "blocked"` (landed 2026-09-15, ADV-R06) | `blocked` is a **first-class reader class**: "user-gated (status=blocked with a blocked_reason). Not work-to-spawn, but not idle-proof either." |
| `internal/scheduler/board_awareness.go` (d) | `blocked` rows "are USER-GATED and stay blocked until an operator unblocks; CountPending below deliberately counts only status=="pending", never blocked rows." |
| `boardctl` (canonical writer) | **accepts** the state: `boardctl create --id PROBE-1 … --status blocked` printed `created task PROBE-1`; `boardctl -C . list --status blocked` lists the 4 rows; `boardctl validate` prints `RESULT: OK`; `boardctl sweep-status --decide` reports only `guard_result`/`ci_result` prose, never `status` |
| `docs/troubleshooting-scheduling-errors.md:698` | "Off-vocabulary statuses: re-spell to `pending`/`in_progress`/`complete`" — sides with the gate |

Ordering: the reader class (ADV-R06, 2026-09-15) predates the writer gate (SCHED-GAP-164, 2026-09-18), and the gate's own comment names the parked spellings it means to catch (`todo, open, rework, new, claimed, ready`) — `blocked` is not among them. The allowlist omission reads as an oversight of that closure, not a policy decision.

Why the obvious data repair is the wrong arm: re-spelling the four rows to `pending` (the precedent used at `769b10d3`, which filed INT-CI-4182 for the identical class) makes four **owner-gated** rows count in `CountPending` and therefore dispatchable — precisely the dispatchability hazard the PM lane was closing. Widening the writer vocabulary to include `blocked` unreds the gate and preserves the documented semantics; `RELEASE-007` (`06adb23f`) is the template for exactly this kind of lockstep vocabulary widening (`in_progress`).

Left as a RELEASE-044 row for the foreman: the releng lane verifies, it does not change product gates.

## Released artifact (v1.14.1) — all five acceptance arms re-verified this tick

```
1) git show origin/main:docs/release-evidence/2026-11-07-v1.14.1.md | wc -l   -> 19
   git cat-file -t v1.14.1                                                    -> tag
   git rev-parse 'v1.14.1^{commit}'                                           -> de573541d21d8402fedcf8f7396702066546c96e
2) gh release view v1.14.1 --json tagName,isDraft,isPrerelease,assets --jq …  -> v1.14.1  false  false  9
3) gh run view 42392333708 --json status,conclusion                           -> completed  success
4) gh release download v1.14.1 (9 files) && sha256sum -c SHA256SUMS           -> 8/8 OK, rc=0
5) gh run view 42392333648 --json status,conclusion                           -> completed  cancelled
   foreman_note records it as a follow-up and cites pre-tag CI Pipeline 42389770383
```

Verification scratch dir for arm 4: `/home/kara/.hermes/cache/scratch/releng-arm4-mOBV`.

## Gates, dogfood, dependencies, deployed artifact

- GitReins: the only verdicts dated 2026-11-07 are bound to other SHAs (`DOC-26 a7654e57`, `DOC-27 ff1ed01a`, `DOC-29 75d37f0e`, `INT-CI-4187 e054fa46`, `QA-CHS-187 972f11c8`, `README-19 7446d2c3`); `QA-CHS-187` covers `972f11c8`, which is inside the released range. **No verdict is bound to `de573541` or `6ae3193c`** — UNVERIFIED for the release tip, hosted CI carried the gate.
- Dogfood: no entry references `de573541` or `v1.14.1` in `.qa/` or the repo. **UNVERIFIED** for the candidate (unchanged from the cut record).
- Dependencies: no dependency commits in the range; no new advisories observed this tick.
- Deployed binary: **STALE**. Live `schedulerd` (pid 3347652, uptime 11h32m) runs `/home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd`, built `2026-11-07T05:42:56Z`, reporting `v1.14.0-40-gd0dad6d9-dirty` against released `v1.14.1`. Tracked by open rows `SCHED-GAP-162` (P1) and `SCHED-GAP-16994` (P1) — not duplicated here.
- Board: 813 rows, 91 open, 0 duplicate ids, `boardctl validate` `RESULT: OK (918 warnings, unchanged)`.

## Board writes made by this tick

- `RELEASE-043` → **complete**: five arms above verified with raw output; gate green cited as pre-tag CI Pipeline `42389770383`; tag-triggered duplicate `42392333648` recorded as cancelled, never as green.
- `RELEASE-044` → **pending, P1**: the `status=blocked` writer/reader/gate disagreement that reds exact-head CI, with the repair contract (lockstep vocabulary widening, regression fixture, docs correction) and an explicit prohibition on re-spelling the four owner-gated rows to `pending`.

## Release URL / not created

No release created this tick (zero material commits). Last release: https://github.com/coding-hermes/scheduler/releases/tag/v1.14.1

## Pass 2 — this tick was re-delivered; state re-probed at the newer tip

Same tick id `coding-hermes-scheduler-releng-2026-11-07-15-25-21` was delivered again at
`2026-11-07T17:31Z`. Pass 1's record above already landed (this artifact + `RELEASE-044`), so this
pass re-probed instead of re-deriving, per the re-gating rule.

- Pass-1 tip: `6ae3193cc58ec3c7de4c1d98677a7d4aba5c1fe8`.
- Pass-2 verified tip (`git ls-remote origin refs/heads/main`, local `HEAD`, detached worktree `/tmp/releng-sched-E2LoeT` at that SHA, 0 dirty entries): `dd4b0827ce586dbca358e8222fa668ad074ab8bb`.
- Delta between the two: 2 commits, both bookkeeping — `176e2e30` (`chore(releng): readiness sweep`) and `dd4b0827` (`board(releng): close RELEASE-043, file RELEASE-044`). `git diff --name-only 6ae3193c..dd4b0827` = the two board JSONL files only. **Zero code files.**
- Range `v1.14.1..dd4b0827` = 6 commits / 4 files: `.coding-hermes/board/events.jsonl`, `.coding-hermes/board/tasks.jsonl`, `docs/release-evidence/2026-11-07-v1.14.1.md`, this artifact. Census `pm`=3, `chore`=2, `board`=1, **feat=0 fix=0 perf=0**, no `BREAKING CHANGE` / `type!` in any body. Decision unchanged: **NOT DUE** (rule: zero material commits).

### Gate re-probe at the newer tip — still RED, one violation wider

`python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only` in the
detached worktree at `dd4b0827`:

```
VIOLATION board-vocab SCHED-GAP-128: status=blocked
VIOLATION board-vocab REL-GUARD-001: status=blocked
VIOLATION board-vocab SCHED-GAP-16934: status=blocked
VIOLATION board-vocab SCHED-GAP-16937: status=blocked
INFO board-vocab .coding-hermes/board/tasks.jsonl: 814 row(s) scanned, 0 legacy closed spelling(s)
FAIL — 4 violation(s) (board-only mode: fleet checks 1-7 not run — no local DB/TOML)
rc=1
```

CI on the newer tip: `dd4b0827` carries `[ci skip]` and a board-only diff, so it has **no run by
construction** (`gh run list --limit 6` newest entry is still `42392863396`, `CI`,
`completed/failure`, headSha `517eca42`). The deciding receipt for the gate is therefore unchanged and
still red — `RELEASE-044` stands as the repair contract, not re-filed.

### Released artifact re-verified (unchanged)

- Annotated tag `v1.14.1` → object `77f64d47…` → commit `de573541d21d8402fedcf8f7396702066546c96e` (remote `refs/tags/v1.14.1^{}` agrees).
- Release object: `draft=false pre=false`, published `2026-11-07T16:39:24Z`, **9 assets** (`schedulerd-{linux,darwin}-{amd64,arm64}`, `migrate-{linux,darwin}-{amd64,arm64}`, `SHA256SUMS`).

### Dependencies — and why a row was filed

- `govulncheck ./...` → `No vulnerabilities found.` (rc=0), so no advisory fires the release-now rule.
- `go list -u -m all` → **16 modules** with updates, all minor/patch, no majors: direct `modernc.org/sqlite v1.60.1 → v1.62.0`; indirect `golang.org/x/net v0.59.0 → v0.62.0`, `golang.org/x/sys v0.48.0 → v0.49.0`, `golang.org/x/tools v0.51.0 → v0.52.0`, `modernc.org/libc v1.77.1 → v1.80.1`, `goldmark v1.4.13 → v1.8.7`, plus pprof/telemetry/exp/mod/gc/cc/ccgo/regexp2/go-cmp/demangle.
- Last full module refresh: `c2922ea6` (2026-10-06, DEPS-2, 12 modules) — **32 days ago**, so the monthly minor window is open. It cannot be exercised while exact-SHA CI is red (no gate can certify the refresh), so it is filed as `RELEASE-045`, `--depends-on RELEASE-044`.

### Deployed binary — stale, already tracked, not duplicated

Live `schedulerd` pid 3347652 (uptime 11h48m) runs the repo-local `bin/schedulerd`, whose `go version -m`
reads `v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`, `vcs.revision=d0dad6d9ede0`, `vcs.modified=true`.
That is 9 non-board commits behind `v1.14.1`, including the code change `6606f856` ("spawn: one board
rule, and the test that keeps it"). Class = STALE (not ABSENT). Tracked by open `SCHED-GAP-162` and
`SCHED-GAP-16994`; the row premise there ("0 code commits behind") is no longer true and is noted
rather than duplicated.

## Pass 3 — same tick re-delivered again; re-probed at the newer tip, and one carried justification corrected

Third delivery of tick id `coding-hermes-scheduler-releng-2026-11-07-15-25-21`
(`2026-11-07T20:25Z`). Passes 1 and 2 are recorded above and their writes already landed
(RELEASE-043 closed, RELEASE-044/045 filed). This pass re-probes instead of re-deriving.

- Pass-1 tip: `6ae3193cc58ec3c7de4c1d98677a7d4aba5c1fe8`
- Pass-2 tip: `dd4b0827ce586dbca358e8222fa668ad074ab8bb`
- **Pass-3 verified tip: `7d10a0ba81b520b0d23d4daa30b042744a816d09`** — `git ls-remote origin
  refs/heads/main`, local `HEAD` after fetch, and the detached worktree
  `/home/kara/.hermes/cache/scratch/releng-chs-20261107-1525` (0 dirty entries, root
  `/home/kara/.hermes/cache/scratch/releng-chs-20261107-1525`, remote
  `https://github.com/coding-hermes/scheduler.git`) all agree.
- The canonical checkout is **not** this tip: it sits at `517eca42` (6 behind) and is dirty —
  see the new finding at the end.

Delta pass-2 → pass-3 is **1 commit**, `7d10a0ba` (`chore(releng): re-probe release gate at
tip dd4b0827 — still RED …`), touching only the two board JSONL files and this artifact.
`git diff --name-only dd4b0827..7d10a0ba` lists no code path.

Range `v1.14.1..7d10a0ba` = **7 non-merge commits / 4 files**, every one bookkeeping:

```
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
docs/release-evidence/2026-11-07-v1.14.1.md
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

Census by conventional prefix: `pm`=3, `chore`=3, `board`=1 — **feat=0, fix=0, perf=0**, no
`BREAKING CHANGE:` footer and no `type!` marker in any body. Classified counts reconcile with
`git rev-list --no-merges --count v1.14.1..7d10a0ba` = 7. Decision unchanged: **NOT DUE**
(zero material commits).

### Gate re-probe at `7d10a0ba` — still RED, same four violations

```
$ python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only
VIOLATION board-vocab SCHED-GAP-128: status=blocked
VIOLATION board-vocab REL-GUARD-001: status=blocked
VIOLATION board-vocab SCHED-GAP-16934: status=blocked
VIOLATION board-vocab SCHED-GAP-16937: status=blocked
INFO board-vocab …: 815 row(s) scanned, 0 legacy closed spelling(s)
FAIL — 4 violation(s) (board-only mode: fleet checks 1-7 not run — no local DB/TOML)
$ echo $?   -> 1
$ bash scripts/check-invariants.sh >/dev/null 2>&1; echo $?   -> 1
```

`7d10a0ba` itself has **no run by construction** (`[ci skip]` + board-only diff); the newest
run in the repository is still `42392863396` (`CI`, headSha `517eca42`,
completed/failure). Per-job state of that deciding run, read this pass:

```
build: completed/failure            <- the only red job
lint: completed/success             race-blocks: completed/success
race-scheduler: completed/success   multiarch: 9 builds/verifies completed/success
multiarch / Container (multi-arch): completed/skipped   (tag-only)
multiarch / Release: completed/skipped                  (tag-only)
```

Board census at the tip: 723 complete / 88 pending / **4 blocked** — the four blocked rows
are exactly the four violations, so the red is fully explained by that one class.

### CORRECTION to a carried claim: the docs do NOT describe this as an oversight

RELEASE-044's reasoning advances an ordering argument — the reader class (`9d05dcfa`,
2026-09-15, ADV-R06) predates the writer gate (`ab6e3a64`, 2026-09-18, SCHED-GAP-164),
therefore "the allowlist omission reads as an oversight of that closure, not a policy
decision". It cites `docs/troubleshooting-scheduling-errors.md:698` only as "sides with the
gate".

That inference is **refuted by the repository**. The sentence immediately following the
quoted one, at the same line, reads:

> parked spellings (`todo`, `open`, …) are writer bugs and must be re-filed as `pending` or
> deliberately closed. **`blocked` and `retired` are outside the vocabulary on purpose and
> will keep failing the gate until re-filed.**

`git log -S'are outside the vocabulary on purpose'` binds it to `a08202ab` (2026-09-20,
SCHED-GAP-153 troubleshooting taxonomy) — i.e. it was written **after both** the reader class
and the writer gate, and it names `blocked` specifically. The exclusion is a recorded
decision, not a closure oversight, so RELEASE-044 as written would have a foreman reverse a
deliberate policy while describing the change as a bug fix.

What still argues for widening the vocabulary is a different, stronger surface — the fleet
harness doctrine this repo's rows are operated under:

- `owner-gated-foreman-ticks`: "**A genuine external gate must be `status: blocked`, never
  `pending`.** … a *pending* row is precisely what waives that lane's cooldown … Persisting it
  as pending is not caution; it is the idle-spin generator." and "**`blocked` and `pending`
  are admission controls, not cosmetic labels.**"
- `coding-hermes-docs-audit`: prescribes `--status blocked --blocked-reason "DUPLICATE of …"`
  as the parking mechanism, because the writer's vocabulary has no `duplicate` value it uses.
- `boardctl` accepts the state (probed by pass 1: `created task PROBE-1`; `validate` OK).

So the four parked rows are spelled the way the fleet's own operating doctrine mandates, and
the gate's allowlist is the surface that must move — but the change is a **policy reversal to
be recorded as one**, not an oversight repair. RELEASE-044's reasoning has been rewritten to
carry that correction (the repair contract itself is unchanged: lockstep Go+Python widening,
the parity pin in `internal/scheduler/board_vocab_test.go:227`, a regression fixture, and the
docs sentence corrected in the same change). The prohibition on re-spelling the four
owner-gated rows to `pending` stands and is now doubly supported.

### Released artifact — unchanged, still intact

- Annotated tag `v1.14.1` → commit `de573541d21d8402fedcf8f7396702066546c96e`; Release object
  non-draft, non-prerelease, 9 assets; tag-triggered Release run `42392333708`
  completed/success. No re-verification delta this pass beyond the tag→commit binding.
- Version surfaces agree with the released tag: `CHANGELOG.md:36` = `## [1.14.1] — 2026-11-07`
  with `## [Unreleased] — 2026-11-07` above it, `internal/version/version.go` `Version = "dev"`
  (ldflags-injected at build, as designed).

### Deployed binary — STALE, number superseded

`schedulerd` pid 3347652 still runs `/home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd`,
`go version -m` = `v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`, `vcs.revision=d0dad6d9ede0`,
`vcs.modified=true`, built `2026-11-07T00:42Z`. `git rev-list --count d0dad6d9..v1.14.1`
= **20 commits** behind the released tag (RELEASE-044's "9 non-board commits behind" is
superseded). Class = STALE, not ABSENT. Still owned by open `SCHED-GAP-162` and
`SCHED-GAP-16994`; not duplicated.

### NEW finding filed this pass — RELEASE-046

The canonical checkout `/home/kara/coding-hermes-scheduler/coding-herms-scheduler` is at
`517eca42` (6 commits behind `origin/main`) **and** carries an uncommitted row
`SCHED-GAP-17000` on that stale board base (813 rows on disk vs 815 at the tip). Any commit
made from that tree on its current base rewrites `tasks.jsonl` back over the releng rows that
landed in `176e2e30`, `b720bcee`, `dd4b0827` and `7d10a0ba` — including RELEASE-044/045 and
this record. Filed as `RELEASE-046` (P2) because it is a release-record integrity hazard, not
a scheduler defect.

### Dependencies

Unchanged from pass 2: 16 modules behind, all minor/patch, `govulncheck ./...` clean,
monthly window open 32 days, blocked behind RELEASE-044 (`RELEASE-045`, P2). No new advisory
observed this pass.

### Decision this pass

No release created. **NOT DUE** — zero material commits in `v1.14.1..7d10a0ba`, and the
exact-SHA code gate is red (`42392863396`), so even a due cut would be blocked. `RELEASE-044`
(P1) remains the single blocking row; `RELEASE-045` waits behind it.
