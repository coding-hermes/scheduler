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

## Pass 4 — fourth delivery; the delta turned material, two defects came and went, one blocker remains

Fourth delivery of tick id `coding-hermes-scheduler-releng-2026-11-07-15-25-21`
(`2026-11-07T18:50Z`). Passes 1–3 are above and their writes landed. This pass re-probed the tip
and **the tip moved five times while it was reading and landing** — a feature, a test repair, a
wave close, a clock-seam fix and a RELEASE-044 dispatch all landed from sibling lanes. Only the
final tip is a decision input; the intermediate states are recorded because two of them were real
defects this pass caught.

Tip movement during this pass:

| point | SHA | commits since `v1.14.1` | what it was |
|---|---|---|---|
| pass-3 tip (start) | `7d10a0ba81b520b0d23d4daa30b042744a816d09` | 7 | bookkeeping only |
| mid-pass re-read | `bfc235ffd9eebaaa19019423ec4838e3093e4bd4` | 10 | bookkeeping only |
| feature lands | `7ba9c42ed9164ac92b48219085809f53872989db` | 12 | 1 feat (`76264ed9`) |
| + test repair, wave close | `6d063ff13f45ae2647acf30677384392d6efaa1c` | 14 | 1 feat + 1 test fix |
| + clock fix, RELEASE-044 dispatched | `2de97425ad5bdb587aeef6bf971545aa80cfbb04` | 21 | 1 feat + 1 test fix + 1 fix |
| **final verified tip** | **`0dcd76d8ff2d1597fe1044ae255f873f7cd457d4`** | **24** | **+ RELEASE-044 vocabulary fix (gate now PASSES)** |

`0dcd76d8` = `git ls-remote origin refs/heads/main`, local `HEAD` after fetch, and the clean
detached worktree `/home/kara/.hermes/cache/scratch/releng-chs-p4e-140719` (root and remote
asserted, 0 dirty entries) all agree. Landing sequence: `76264ed9` (the feature) →
`9ee4cc70` (gateway-test race repair) → `95cc9b8c` `fix(clock): route dry-run throttle timestamp
through the clock seam (INT-CI-4188)` → `2de97425` (`board: dispatch RELEASE-044`) → `4632fe79`
`fix(board): admit status=blocked in the writer vocabulary, Go and Python in lockstep` →
`f2e5233c` (close INT-CI-4188) → `0dcd76d8` (finish the RELEASE-044 docs correction).

Range `v1.14.1..0dcd76d8` = **24 non-merge commits / 27 files**. Census `board`=8, `foreman`=4,
`chore`=4, `pm`=3, `fix`=2, `test`=1, `feat`=1, `docs`=1; `BREAKING CHANGE`/`type!` occurrences =
**0**. 8+4+4+3+2+1+1+1 = 24 = `git rev-list --no-merges --count v1.14.1..0dcd76d8`, so the census
reconciles.

### Decision 1 — a MINOR release is due (`v1.15.0`); the board gate cleared INSIDE this pass

`76264ed9` `feat(scheduler): dry-run throttle — warn at five, suspend at ten` (15 files, +914/−22)
is a minor trigger, so passes 1–3's "not due" is superseded: `v1.14.1` no longer describes the
shipped tree and the bump is **minor**, not a patch.

`RELEASE-044` — the board-vocabulary gate that blocked every cut today — was **repaired inside this
pass** by the worker dispatched at `2de97425`: `4632fe79` `fix(board): admit status=blocked in the
writer vocabulary, Go and Python in lockstep` (+ `f2e5233c` closing INT-CI-4188, `0dcd76d8`
finishing the docs correction). Verified in the clean candidate worktree at the delivered tip:

```
$ python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only
INFO board-vocab .coding-hermes/board/tasks.jsonl: 818 row(s) scanned, 0 legacy closed spelling(s)
PASS — 0 violation(s)   rc=0
```

and the hosted receipt agrees: the `build` job of CI run `42404019008` at `0dcd76d8` is
`completed/success` — the first green `build` of the day, after six red ones.

### CI truth at `0dcd76d8` — gate green, product gate green

| run | workflow | state at write time | red jobs |
|---|---|---|---|
| `42404019008` | CI | `in_progress` — `build`, `lint`, `race-blocks`, `Verify (amd64)`, 6 × `Build <platform>` all `completed/success` | none |
| `42404018443` | CI Pipeline | `in_progress` | none observed |

Earlier same-day receipts, kept for the record and superseded by the above:
`42402573716` (CI at `2de97425`, red: `build` only — board-vocab) and its green product counterpart
`42402573052` (CI Pipeline, `completed/success`, all six jobs); `42400367923`/`42400367005` (at
`6d063ff1`, red: `build` + `Verify (amd64)` / `Test (Go 1.26)`); `42399101030`/`42399100369` (at
`7ba9c42e`, same shape).

Read the surfaces separately. At `2de97425` the product gate (`CI Pipeline`: unit tests, lint, race
detector, integration, smoke, binary build) was green end to end while the `CI` workflow's `build`
job was red on the board vocabulary — a green product workflow never certifies the board predicate,
and vice versa. At `0dcd76d8` both predicates have evidence of being satisfied: the board battery
passes locally with `rc=0` and its hosted job is `completed/success`, while the product jobs were
last seen green at the same SHA family (`42402573052` at `2de97425`, whose tree differs only by the
vocabulary fix's own tests).

### Why the tag was NOT created in this pass

`v1.15.0` is authorized and, as of `0dcd76d8`, unblocked — but a cut is not a tag: it needs a
populated `[Unreleased]` section (today it is empty), then `make release-prep VERSION=v1.15.0`, and
each of those is a commit whose own exact-SHA CI must go green before the next step is allowed
(`docs/releases.md` §3; RELEASE-041 documents the empty-`[Unreleased]` trap). That is two CI cycles
on a branch that moved **six times during this single pass** — the certified window was minutes wide
and the tip changed underneath every probe. Cutting inside that churn would have meant tagging a SHA
whose CI I had not read, which is the one thing the release discipline forbids; and the tag-triggered
`release.yaml` is the only publish path, so a half-cut leaves either an orphan tag or a Release
whose assets no run produced.

Recorded next step for the next releng pass, in order: (1) re-anchor on the settled tip, (2) land
the `[Unreleased]` body as its own pushed commit (derived from `make release-prep … --draft-from-range`),
(3) wait for that SHA's CI green, (4) run `make release-prep VERSION=v1.15.0` and land it,
(5) wait for that SHA's CI green, (6) tag that exact commit and let `release.yaml` publish,
(7) verify 9 assets + `SHA256SUMS`. No owner approval is needed for a minor cut; this is a
scheduling judgment, stated plainly rather than dressed as a permission gate.

Note on the history kept above: at the intermediate tips `7ba9c42e` and `6d063ff1` the CI workflow
was red on **two** arms, `build` *and* `multiarch / Verify (amd64)` (`go test -short`), and the CI
Pipeline was red on `Test (Go 1.26)`. Those receipts are `42399101030`/`42399100369` (at
`7ba9c42e`) and `42400367923`/`42400367005` (at `6d063ff1`). Both extra reds were genuine defects
in the new feature, both were found by this pass, and both were repaired by sibling lanes before
this record landed:

- **Clock seam violation** — `76264ed9` added
  `internal/scheduler/dry_run_throttle.go:289 nowUTCString() { return time.Now().UTC().Format(time.RFC3339) }`,
  a direct stdlib clock call, which fails `TestNoDirectStdlibClockCallsOutsideClock` in
  `internal/clock` (the class `RELEASE-031` repaired once already). Red-proof: at `bfc235ff`
  (pre-feature) the test prints `ok 0.105s`; at `7ba9c42e` and `6d063ff1` it prints `FAIL` naming
  `dry_run_throttle.go:289`; at `2de97425` it prints `ok 0.101s` again after `95cc9b8c`.
- **Gateway test read-once race** — `TestGatewayHealthGate_Recovery` failed in the hosted unit job
  at `7ba9c42e` but passed locally at that same SHA; `9ee4cc70` repaired it by polling the
  recovery tick's dry-run charge instead of reading it once, and it passes at `2de97425`.

Because both were fixed before this record could land, **no repair row was filed for either** —
a pending row for already-fixed work would dispatch a phantom task. The finding is carried here
instead, with the introducing and repairing commits named. One lead is still unverified and is
recorded as a lead only: the hosted `Unit Tests` log that failed at `6d063ff1` also carries
`DRY-RUN-THROTTLE … SQL logic error: no such column: dry_run_throttle`, and no row claims it is
fixed.

### Released artifact `v1.14.1` — unchanged and intact

- Annotated tag object `77f64d473bce961b756509744ebbc620ec962481` → commit
  `de573541d21d8402fedcf8f7396702066546c96e`; remote `refs/tags/v1.14.1^{}` agrees.
- Release object `draft=false pre=false`, published `2026-11-07T16:39:24Z`, 9 assets;
  tag-triggered `Release` run `42392333708` = `completed/success`.
- Version surfaces: `CHANGELOG.md:34` `## [Unreleased] — 2026-11-07` is **empty** while a feature
  now sits above the released tag. That is the expected `--draft-from-range` shape (`RELEASE-041`),
  and it must be populated as its own pushed commit before the `v1.15.0` prep commit.

### Gates, dependencies, deployed artifact

- GitReins: the seven verdicts dated 2026-11-07 bind `972f11c8` (QA-CHS-187 ×2), `a7654e57`
  (DOC-26), `ff1ed01a` (DOC-27), `75d37f0e` (DOC-29), `7446d2c3` (README-19), `e054fa46`
  (INT-CI-4187). **None binds the candidate** (`2de97425`, `76264ed9`, `95cc9b8c`) → UNVERIFIED
  for the candidate SHA; the hosted exact-SHA suite is the gate that spoke.
- Dependencies: 16 modules behind, all minor/patch (`modernc.org/sqlite v1.60.1 → v1.62.0` the only
  direct bump); `govulncheck ./...` → `No vulnerabilities found.` at this tip. `RELEASE-045` (P2)
  keeps waiting behind the red gate.
- Deployed binary: **STALE**, not absent. `schedulerd` pid 3347652 still runs the repo-local
  `bin/schedulerd` (`v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`, `vcs.revision=d0dad6d9ede0`,
  `vcs.modified=true`, built `2026-11-07T05:42:56Z`) — 20 commits behind the released tag and now
  further behind an unreleased feature. Owned by `SCHED-GAP-162` and `SCHED-GAP-16994` (both
  pending/P1); not duplicated here.
- Dogfood: nothing in `.qa/` references the candidate. **UNVERIFIED**.

### `RELEASE-046` — arms 1–3 hold, arm 5 holds, arm 4 still does not; row stays open

```
ARM 1  git rev-parse HEAD == git rev-parse origin/main == 0dcd76d8…   SATISFIED (checkout clean, 0/0 divergence)
ARM 2  landing commit 7ba9c42e: tasks.jsonl +1/-0, removed lines = 0  SATISFIED (SCHED-GAP-17000 landed onto the fetched tip)
ARM 3  origin/main blob readback -> RELEASE-043, RELEASE-044, RELEASE-045, RELEASE-046,
       SCHED-GAP-17000 all present                                   SATISFIED
ARM 4  installed hook that REFUSES a stale-base board commit:
       hooks dir holds samples only — nothing installed                NOT SATISFIED
ARM 5  SCHED-GAP-162 and SCHED-GAP-16994 still open                    SATISFIED (both pending/P1)
```

The hazard was observed live earlier in this pass — the canonical checkout was genuinely mid-rebase
with `UU` conflicts on both board files and a feature commit on a stale base — and it quiesced
because the sibling finished and pushed. No row was closed: four of five arms hold, enforcement
does not.

### Decision this pass

**`v1.15.0` is due; the blockers cleared during the pass; no tag was created (see the §"Why the tag
was NOT created" reasoning above).** Zero tags, zero Release objects, no artifact uploaded; the
`v1.14.1` Release object is untouched. State at the delivered tip `0dcd76d8`: board battery
`PASS — 0 violation(s)` locally and `build` green hosted (`42404019008`); `RELEASE-044` fixed by
`4632fe79`; the clock and gateway defects fixed by `95cc9b8c` / `9ee4cc70`. Open rows:
`RELEASE-045` (deps, waiting on a settled cut), `RELEASE-046` (checkout integrity — quiescent, its
enforcement arm unmet).

### Pass-4 addendum — CI terminalized green after the record was first written

Two claims above were written mid-run and are corrected here with the terminal receipts:

- **`42404019008` (`CI` at `0dcd76d8`) is now `completed/success`** — not merely in progress. Every
  job is green: `build` (the invariant battery that had been red all day), `lint`, `race-blocks`,
  `race-scheduler`, `multiarch / Verify (amd64)`, `Verify (arm64, native)` and all six
  `Build <platform>` jobs. `Container (multi-arch)` and `Release` are skipped by the tag-only
  filter. This is the **first fully green `CI` workflow on a code-bearing SHA today**, and it is
  the certified candidate SHA for the `v1.15.0` cut.
- `42404018443` (`CI Pipeline` at `0dcd76d8`) was still `in_progress` when this addendum was written
  (`Lint` success; `Test (Go 1.26)` and `Race Detector (internal/api)` running). Recorded as
  PENDING, not as green — the product-line receipt for this SHA is not yet in hand.
- Tips landed after this record's first landing — `a748c11e`, `eff904b2`, `66f4fc7a` — are
  **board/harness only** (`git diff --name-only 1c0cb890..origin/main` lists
  `.coding-hermes/board/{events,tasks}.jsonl` and `.gitreins/tasks.yaml`), so the code tree and its
  census are unchanged and the candidate SHA for a cut remains `0dcd76d8`.
- Board rows: `RELEASE-044` is now `in_progress` on its CI-Pipeline arm (per `eff904b2`), and the
  serial foreman close-out is deliberately blocked on that arm rather than closed early — which is
  the correct shape, and it means the release remains gated on that row's completion even though
  the board battery itself passes.

Nothing else in this pass changes: no tag, no Release object, no artifact.


## Pass 5 — fifth delivery, after the release landed: `v1.15.1` cut and verified in this pass

Fifth delivery of tick id `coding-hermes-scheduler-releng-2026-11-07-15-25-21`
(`2026-11-07T20:25Z`). Passes 2–4 are above. Between pass 4 and this pass the
`v1.15.0` cut that pass 4 deferred was **completed by this lane**: prep commit
`9623fbaf` (14:36), Release object published `2026-11-07T20:12:29Z`, 9 assets,
both workflows green on the tag. `RELEASE-047` is `complete`. This pass therefore
re-gated at the post-release tip and cut the follow-on **patch**.

Tip movement during this pass:

| point | SHA | what it was |
|---|---|---|
| start of pass (delivered tip) | `4c9ef809` | `board(releng): close RELEASE-047` — `[ci skip]`, no run |
| **cut commit (tagged)** | **`f6ea23d7`** | `chore(release): v1.15.1` — CHANGELOG body + rollover, 1 file, +12 |
| sibling, after the tag | `c746b77c` | `board: file RELEASE-048` — board only, `[ci skip]` |
| sibling, after the tag | `57a5ae4f` | `board: stamp completed_at on RELEASE-045/047` — board only, `[ci skip]` |

`f6ea23d7` is an ancestor of `origin/main` (`git merge-base --is-ancestor v1.15.1
origin/main` → `rc=0`), so the tag is on the released line and the two later
commits ship in the next cut by design.

### Census `v1.15.0..4c9ef809` — 10 non-merge commits, reconciles

`board`=6, `fix`=2, `chore`=1 (`chore(deps)`), `docs`=1. 6+2+1+1 = 10 =
`git rev-list --no-merges --count v1.15.0..4c9ef809`. `BREAKING CHANGE` /
`type!` occurrences = **0**. Material: the two `fix(board)` commits and the
lockfile-only dependency refresh — the latter is the work `v1.15.0`'s notes
explicitly deferred ("belongs in its own commit and its own gate").

### Decision 1 — a PATCH release is due (`v1.15.1`), rule 5

Not rule 4 (no `feat`), not rule 7 (`v1.15.0` was cut 62 minutes earlier, so no
cadence floor applies). Rule 5 fires: at least one material `fix` since the last
tag with the exact-SHA gates green, and the cut is the gate `v1.15.0` reserved
for the dependency refresh. No owner decision is involved at patch level
(standing releng authority); nothing here is breaking, so no escalation clause
applies.

Carried content: `d2b961a0` (stale-base board-commit refusal), `266cebe9`
(`.gitreins/tasks.yaml` classified as bookkeeping), `81b146b8` (16 Go modules,
1 direct `modernc.org/sqlite v1.60.1 → v1.62.0`, no majors, `govulncheck`
clean).

### Exact-SHA gates at the cut commit `f6ea23d7`

| run | workflow | conclusion | jobs |
|---|---|---|---|
| `42412373587` | CI | **success** | 14 jobs: 12 `success` (incl. `build`, `lint`, `race-scheduler`, `race-blocks`, `multiarch / Verify (amd64)`, `Verify (arm64, native)`, 6 × `Build <platform>`), 2 `skipped` (`Container (multi-arch)`, `Release` — tag-only `if:`) |
| `42412372572` | CI Pipeline | **success** | 7 jobs all `success` (`Test (Go 1.26)`, `Race Detector (internal/api)`, `Lint`, `Simulation Smoke Test`, `Integration Test`, `Build Binaries`, `CI All Clear`) |

The delivered tip `4c9ef809` has **no run by construction** (`[ci skip]`,
board+docs-only), so the gate that decides the tree is the last code-bearing
SHA: `git diff --name-only 81b146b8..4c9ef80` lists only
`.coding-hermes/board/{events,tasks}.jsonl` and
`docs/release-evidence/2026-11-07-v1.15.0.md`, and `81b146b8` carries both
workflows green (`42410159639` CI, `42410158874` CI Pipeline). The cut commit
`f6ea23d7` is CHANGELOG-only (`git show --stat` → `CHANGELOG.md | 12 ++++++++++++`),
so its own two runs above gate a tree byte-identical to `81b146b8` in every
compiled path.

### Local gate — one DEGRADED receipt and one environmental FAIL, both attributed

- The commit hook's `gitreins guard` returned **`Tier 1: DEGRADED PASS`** with
  `go_build/go_lint/go_tests = skipped (No Go files staged)`; by the lane's own
  rule that is **non-evidence** for a tree gate, because a CHANGELOG-only
  changeset selects no Go scope. The hook also warned
  `hook_timeout=300s <= test_timeout=900s` and self-raised its budget to 1020 s
  — a driver finding, not a cut blocker.
- A first local `go test -short -p 1 -count=1 ./...` **FAILED** in
  `internal/scheduler` (153 s) while the box ran at loadavg 11.7. Root cause is
  the known environment trap, not the tree: this shell carries
  `SCHEDULER_OPERATOR_RESTART_APPROVED=1`, which flips the scheduler package's
  fail-closed restart tests. Re-run with the variable unset:
  `env -u SCHEDULER_OPERATOR_RESTART_APPROVED go test -short -count=1
  ./internal/scheduler` → `ok … 214.404s`, `RC=0`. The hosted equivalent
  (`multiarch / Verify (amd64)`, which runs `go test -short`) is `success` at
  the same SHA, so the tree gate is green on both the fixed local run and CI.

### Shipped artifact `v1.15.1` — verified layer by layer

- Annotated tag object `e8ae6455…` → commit `f6ea23d7`; remote
  `refs/tags/v1.15.1` = `e8ae6455…`, `refs/tags/v1.15.1^{}` = `f6ea23d7…`.
  The tag was created on the committed SHA, not on the moving tip.
- `Release` workflow run `42414729229` → `completed/success`; Release object
  `isDraft=false isPrerelease=false`, published `2026-11-07T21:13:45Z`.
- **9 assets** downloaded and `sha256sum -c SHA256SUMS` → `OK` for all 8
  binaries (+ `SHA256SUMS` itself).
- Provenance read from every asset with `go version -m` (no execution):
  `vcs.revision=f6ea23d7` and **`vcs.modified=false` on all 8** — the
  `+dirty`/`vcs.modified=true` defect class that `RELEASE-037` opened is not
  present in this cut.
- The refresh is really inside the bytes: `go version -m
  schedulerd-linux-amd64` reports `mod github.com/coding-hermes/scheduler
  v1.15.1`, `dep modernc.org/sqlite v1.62.0`, `dep modernc.org/libc v1.80.1`,
  `dep golang.org/x/sys v0.49.0`, toolchain `go1.26.10`.
- Tag-triggered `CI Pipeline` (`42414729246`) was still running when this record
  was written; its terminal state is reported in the delivery message, and per
  the lane rule it is not recorded here as green until it concludes.

### Rides forward, not cut-blocking

- **Deployed binary is STALE, not absent**: pid 3347652 runs
  `…/bin/schedulerd` = `v1.14.0-40-gd0dad6d9-dirty`, `build_sha=d0dad6d9`,
  built `2026-11-07T05:42:56Z`, `vcs.modified=true`; `/api/v1/health` agrees
  (`build_sha d0dad6d9`). Now three releases behind. Owned by `SCHED-GAP-162`
  (pending/P1) — not duplicated here.
- **Dogfood: UNVERIFIED** for this candidate — `.qa/` holds only the
  `chaos-probes` script and no result references `f6ea23d7`.
- `RELEASE-045` remains `in_progress` on its tier-2 verdict arm even though its
  hosted CI arm (arm 6) is satisfied by `42410159639` / `42410158874`; the
  refresh is in this release, so the row's remaining arm does not gate the cut.
- `RELEASE-048` (filed by a foreman tick about this lane's ledger accounting) is
  **left untouched** — it is a scheduler-accounting finding, not a release
  finding. This pass's own decision record is filed as `RELEASE-049`.

Nothing in this pass rewrote an existing tag, deleted an asset, or touched the
`v1.15.0` Release object.

## Pass 6 — sixth delivery, 31 minutes after the `v1.15.1` cut: NOT DUE, and the one leg pass 5 left pending is now green

Sixth delivery of tick id `coding-hermes-scheduler-releng-2026-11-07-15-25-21`
(`2026-11-07T21:44Z`). Pass 5 (above) cut `v1.15.1`. This pass re-anchored on
the newer tip and re-probed every claim the earlier passes carried forward.

### Re-anchor — three views agree

| point | SHA | what it was |
|---|---|---|
| delivered tip | `f499c55b` | `board(releng): RELEASE-049 addendum` — `[ci skip]`, no run |
| **cut commit (tagged)** | **`f6ea23d7`** | `chore(release): v1.15.1` — unchanged, still the tag target |
| tag object | `e8ae6455…` | remote `refs/tags/v1.15.1` agrees with the local object |

`git ls-remote origin refs/heads/main` = `f499c55b74bf635ffb460174375669e0832c88c3`,
the detached worktree's `HEAD` = the same, and provider `pushed_at` =
`2026-11-07T21:41:44Z`. All three views agree, so no divergence re-probe was
needed and no earlier count had to be discarded.

### Delta since the cut commit — bookkeeping only

```
$ git diff --name-only f6ea23d7..origin/main
.coding-hermes/board/board.jsonl
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
.gitreins/tasks.yaml
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

No path under `internal/`, `cmd/`, `ops/`, `scripts/` or `Makefile`. The code
tree is byte-identical to the tagged commit.

### Census `v1.15.1..origin/main` — reconciles

```
      5 board
      1 chore
```

5 + 1 = 6 = `git rev-list --no-merges --count v1.15.1..origin/main`. Material:
0 feat, 0 fix, 0 perf, 0 `BREAKING CHANGE`, 0 `type!`. Every commit in the range
carries `[ci skip]`.

### Decision 1 — NOT DUE (rule 3)

Zero material commits since `v1.15.1`, which shipped 31 minutes before this pass
(`2026-11-07T21:13:45Z`). Rule 3 fires before rules 4–7 are reachable, and the
14-day cadence floor of rule 6 cannot apply to a release line this fresh. No tag,
no Release object and no artifact was touched in this pass.

### CI — the leg pass 5 recorded as PENDING is terminal green

Pass 5 wrote: "Tag-triggered `CI Pipeline` (`42414729246`) was still running when
this record was written". Re-probed this pass:

| run | workflow | conclusion | jobs |
|---|---|---|---|
| `42414729246` | CI Pipeline | **completed/success** | 7/7 `success`: `Test (Go 1.26)`, `Race Detector (internal/api)`, `Lint`, `Build Binaries`, `Simulation Smoke Test`, `Integration Test`, `CI All Clear` |
| `42414729229` | Release | **completed/success** | `Release` `success` |

The delivered tip `f499c55b` has **no run by construction** — `[ci skip]` plus a
board/evidence-only diff, so the missing run is a property of the tip. The tree
gate is carried by `f6ea23d7`'s two runs above, and the diff quoted one section
up proves the shipped bytes are unchanged between them.

### Shipped artifact `v1.15.1` — re-verified from freshly downloaded bytes

Re-downloaded into a clean directory this pass, independently of pass 5:

```
sha256sum -c SHA256SUMS   →  OK ×8
   migrate-{darwin,linux}-{amd64,arm64}, schedulerd-{darwin,linux}-{amd64,arm64}
go version -m <each asset> →  mod github.com/coding-hermes/scheduler v1.15.1
                              vcs.revision=f6ea23d757daab8eef1d04f5c6c4f798659d504b
                              vcs.modified=false        (all 8)
```

`git rev-parse 'v1.15.1^{commit}'` = `f6ea23d757daab8eef1d04f5c6c4f798659d504b`
— the revision stamped in the published bytes equals the tagged commit, and the
`+dirty` / `vcs.modified=true` class that `RELEASE-037` opened is absent from all
eight. Release object still `isDraft=false isPrerelease=false` with 9 assets.

### Canonical checkout — a new in-flight writer, and why this pass ran detached

At pass 1 the canonical checkout carried an uncommitted DRY-RUN THROTTLE worker.
It now carries a **different** uncommitted changeset, and it was being written
while this pass ran:

```
 M .gitignore, Makefile, CHANGELOG.md, docs/releases.md,
 M internal/database/migrations.go, internal/scheduler/lifecycle.go,
 M internal/scheduler/tick_process.go,
 M .coding-hermes/board/{tasks,events}.jsonl, .gitreins/tasks.yaml
?? cmd/reconcile-ticks/  internal/scheduler/tick_reconcile.go
?? internal/scheduler/tick_reconcile_test.go  scripts/release-claim.sh
```

MTimes cluster at `16:34:38`–`16:39:55 -0500` against a clock of
`16:42:53 -0500`, and an active session process is polling `git status` in that
tree; `.git/index.lock` is absent. This is the implementation of `RELEASE-048`'s
arms (`reconcile-ticks` → arms 1–2, `release-claim.sh` → arm 3). It is **not**
release content: it is uncommitted, and `CHANGELOG.md` + `docs/releases.md` +
`Makefile` being dirty on the release line is precisely the state in which a cut
must not be attempted. No file in that tree was touched by this pass; every
probe above ran in `/home/kara/.hermes/cache/scratch/releng-pass6-f499c55` at
`f499c55b`, with 0 dirty entries and its root plus remote asserted before use.

### Deployed binary — still STALE, now two releases behind

- pid `3347652`, started `2026-11-07 00:42:57 -0500`, elapsed `16h00m`.
- Executable `…/bin/schedulerd`, mtime `2026-11-07T00:42:57-05:00`.
- `go version -m` → `mod github.com/coding-hermes/scheduler
  v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`,
  `vcs.revision=d0dad6d9ede03d15927ec5d249aee9c9ee41f73c`, `vcs.modified=true`.
- `d0dad6d9` **is** an ancestor of `origin/main` (`git merge-base --is-ancestor`
  → `rc=0`), so this is STALE, not a foreign or forked build.

The live scheduler is therefore missing **`v1.15.0` and `v1.15.1`** (two
releases) and runs a binary stamped `+dirty`. Owned by `SCHED-GAP-162`
(pending/P1) and `SCHED-GAP-16994` — not duplicated here, the same rule pass 5
applied. `SCHED-GAP-162`'s title still quotes an older live build
(`v1.12.0-684-gc1f2f033`); the measurement above supersedes that number, and the
row was last updated by a PM lane at `2026-11-07T19:14:26Z`, before the daemon's
current build existed.

### Open-row census (scope: every non-terminal row, `blocked` and `review` included)

`731 complete`, `4 blocked`, `86 pending` → **90 non-terminal**. Six are `P0`:
`SEC-002` (owner-gated credential rotation) plus
`SCHED-GAP-16956` / `16960` / `16962` / `16964` / `16966` (node-provisioning
program rows). None names a defect in shipped code, so none is a tree or cut
blocker; they are recorded here so the next pass does not re-derive the census.

### Rows this pass

- `RELEASE-048` — **unchanged and still pending**, and its premise still holds
  live: `GET /api/v1/projects` reports `coding-hermes-scheduler-releng`
  `last_tick_status=failed`, `last_tick_completed=2026-11-07T13:25:22-05:00`.
  Its implementation is the uncommitted changeset described above. Not touched.
- `RELEASE-049` — the canonical row for this release line; extended with this
  pass's delta and the new artifact fingerprint rather than filing a parallel row
  for an unchanged range.
- **No new `RELEASE-*` row is filed.** Zero material commits since the last tag
  means there is nothing for a foreman to execute, and a pending row for a
  not-due release would dispatch a phantom task.

Nothing in this pass rewrote a tag, deleted an asset, created a Release object,
or touched the `v1.15.0` / `v1.15.1` Release objects.

### Pass-6 addendum — one posture row WAS filed after the record above was drafted

The section "Rows this pass" above says no new `RELEASE-*` row was filed. That
was true when it was written and is corrected here: the posture sweep in §6 of
the release skill ran after that draft, found an unclaimed gap, and `RELEASE-050`
was created (P2, pending).

- **`RELEASE-050` — public repo carries no server-side secret scanning.**
  `gh api repos/coding-hermes/scheduler --jq '.security_and_analysis'` →
  `{"secret_scanning":{"status":"disabled"},"secret_scanning_push_protection":{"status":"disabled"}}`
  on a `visibility=public` repo that publishes 8 binaries plus `SHA256SUMS` per
  Release. Both switches are free for public repositories; enabling them needs
  repository admin, which this lane does not hold, so the row is an owner action
  with three verifiable arms.
- It is **not** a duplicate: the committed board carries no task whose title
  matches `secret scanning` or `push protection`, and the only `/posture/i` hits
  (`SCHED-GAP-1739` MCP deny-all, `DOGFOOD-029` fed/read auth) are unrelated.
- The governance half of the same probe — `required_status_checks=null`,
  `enforce_admins=false` on `main` — is deliberately left on `REL-GUARD-001`
  rather than folded in here, because forge/tag policy is an owner decision
  already parked there.
- It does not change Decision 1: posture is not a cut gate, and nothing was due
  in this pass either way.

No other row was created, and no existing row was closed, retitled or
reprioritised in this pass.

## Pass 7 — seventh delivery, 34 minutes after the `v1.15.1` cut: NOT DUE, artifact and provenance re-read, one new unowned binary-surface finding

Probes began `2026-11-07T21:47Z`. Every command in this pass ran in the detached
worktree `/var/tmp/hermes-scratch/releng-sched-AmkbFJ`, created with
`git worktree add --detach` at the fetched `origin/main`. Its root plus remote
were asserted before use (`/var/tmp/hermes-scratch/releng-sched-AmkbFJ`,
`https://github.com/coding-hermes/scheduler.git`) and it is clean.

### Re-anchor — three views agree at `c6cc9920`

```
git rev-parse origin/main                          → c6cc9920bd6cbfa25d4ca2b80e4c55066dc5d63a
git ls-remote origin refs/heads/main               → c6cc9920bd6cbfa25d4ca2b80e4c55066dc5d63a
gh api repos/coding-hermes/scheduler/commits/main  → c6cc9920bd6cbfa25d4ca2b80e4c55066dc5d63a
git ls-remote --symref origin HEAD                 → ref: refs/heads/main
```

The canonical checkout at `/home/kara/coding-hermes-scheduler/coding-herms-scheduler`
is **still dirty and still 7 commits behind** (`git rev-list --left-right --count
origin/main...HEAD` → `7  0`), carrying the in-flight `RELEASE-048`
implementation. No release action was attempted from it and no file in it was
touched by this pass.

### Delta since the pass-6 tip `f499c55b` — bookkeeping only

```
git diff --name-only f499c55b..c6cc9920
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

Zero code paths. The pass-6 row census and nothing else moved.

### Census `v1.15.1..c6cc9920` — reconciles

```
git rev-list --no-merges --count v1.15.1..origin/main   → 10
git log --format='%s' v1.15.1..origin/main | sed -E 's/^([a-z]+)(\(.*\))?!?:.*/\1/' | sort | uniq -c
      7 board
      3 chore
git log --format='%B' v1.15.1..origin/main | grep -cE 'BREAKING CHANGE|^[a-z]+(\(.*\))?!:'  → 0
```

`7 + 3 = 10` equals `rev-list`, so the census reconciles; every one of the ten
subjects is a `board(releng)` / `chore(releng)` bookkeeping commit carrying
`[ci skip]`, and the material count is **0 feat / 0 fix / 0 perf / 0 breaking**.

### Decision 1 — NOT DUE (rule 3)

Zero material commits since `v1.15.1`, which published
`2026-11-07T21:13:45Z` — 34 minutes before this pass's first probe. Rule 3
(no material change since the last tag) fires before rule 6's 14-day cadence and
before any bump. No tag, Release object, or asset was created, moved, or
deleted.

### CI — the tip has no run by construction; the tree gate is carried by `f6ea23d7`

```
gh run list --repo coding-hermes/scheduler \
  --commit c6cc9920bd6cbfa25d4ca2b80e4c55066dc5d63a --limit 20 --json ...  → []
git diff --name-only f6ea23d7..c6cc9920 | grep -vE '^(\.coding-hermes/|\.gitreins/|docs/|CHANGELOG)'  → no output, rc=1
```

`c6cc9920` carries `[ci skip]` and a board/evidence-only diff, so no push-triggered
run exists for it and none can: `ci.yml` triggers on `push: branches: [main]`
with no path filter, and the marker suppresses the event. The shipped code tree
is **byte-identical** to `f6ea23d7` (the diff above lists only
`.coding-hermes/board/{board,events,tasks}.jsonl`, `.gitreins/tasks.yaml` and
this evidence file), so the release gate is measured on `f6ea23d7`, re-read
this pass:

| Run | Workflow | SHA | Result |
|---|---|---|---|
| 42412373587 | `CI` | `f6ea23d7` | completed/success — 14 jobs: `lint`, `build`, `race-scheduler`, `race-blocks`, `multiarch / Verify (amd64)`, `multiarch / Verify (arm64, native)`, and the six `multiarch / Build <platform>` all success; `multiarch / Container (multi-arch)` and `multiarch / Release` `skipped` (tag-only `if:`) |
| 42412372572 | `CI Pipeline` | `f6ea23d7` | completed/success — 7/7: `Test (Go 1.26)`, `Lint`, `Race Detector (internal/api)`, `Simulation Smoke Test`, `Integration Test`, `Build Binaries`, `CI All Clear` |
| 42414729229 | `Release` (tag) | `f6ea23d7` | completed/success |
| 42414729246 | `CI Pipeline` (tag) | `f6ea23d7` | completed/success — 7/7 |

Both tag-triggered workflows that pass 5 could only record as pending are
terminal green on the tagged SHA, so the `v1.15.1` cut is closed on every layer.

### Version hygiene at the tip — PASS

```
bash scripts/check-version-authority.sh v1.15.1
→ version-authority: PASS — .../CHANGELOG.md names v1.15.1; [Unreleased] present exactly once
grep -nE '^## \[1\.15\.1\]' CHANGELOG.md   → 36:## [1.15.1] — 2026-11-07
```

`## [Unreleased] — 2026-11-07` is present, empty, and sits above the release
heading — the correct post-release rollover, not an un-prepared section.
`docs/releases.md` is a **procedure** document keyed to example versions
(`make release-prep VERSION=v1.4.0`, `scripts/check-version-authority.sh v1.4.0`);
it does not carry a per-release log, so its silence about `v1.15.1` is not a
hygiene gap. The version stamp itself is ldflags-injected
(`internal/version/version.go:26` → `var Version = "dev"`), and the shipped
assets’ `mod … v1.15.1` from pass 5 is the stamp that counts.

### Shipped artifact `v1.15.1` — object re-read, bytes immutable

```
gh release view v1.15.1 --json tagName,isDraft,isPrerelease,publishedAt,assets
→ tag v1.15.1, isDraft false, isPrerelease false, published 2026-11-07T21:13:45Z
→ 9 assets: schedulerd-{linux,darwin}-{amd64,arm64}, migrate-{linux,darwin}-{amd64,arm64},
  SHA256SUMS (720 B)
```

Asset names and sizes are identical to the set pass 5 downloaded and verified
(`sha256sum -c SHA256SUMS` OK ×8, `vcs.revision=f6ea23d7` and
`vcs.modified=false` on all eight, `mod … v1.15.1`), and a published Release’s
bytes are immutable, so that verification still describes these bytes. Pass 6
additionally re-downloaded the set and repeated the checksum pass. Not re-downloaded
a third time in this pass: the object is unchanged and nothing in it can drift.

### Deployed binary — still STALE, still two releases behind

- pid `3347652`, started `Sat Nov 7 00:42:57 2026`, elapsed `16:10:10` at probe.
- Executable `/home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd`,
  mtime `Nov 7 00:42` — the unit’s own `ExecStart` path, rebuilt by
  `ExecStartPre=/home/kara/.hermes/scripts/schedulerd-build.sh` on restart.
- `go version -m` → `mod github.com/coding-hermes/scheduler
  v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`, `vcs.revision=d0dad6d9ede03d15927ec5d249aee9c9ee41f73c`,
  `vcs.modified=true`.

The live scheduler is missing **`v1.15.0` and `v1.15.1`** and runs a `+dirty`
build — unchanged from pass 6, owned by `SCHED-GAP-162` (pending/P1) with its
recurrence row `SCHED-GAP-16994` (complete). Not duplicated here.

### NEW finding filed this pass — `RELEASE-051`: a stale `schedulerd` shadows the canonical build on `PATH`

While enumerating every `scheduler*` binary on the host (deploy-staleness sweep),
`/usr/local/bin/schedulerd` turned out to be a **separate, unreferenced, four-month-old**
binary that any bare `schedulerd` invocation resolves to:

```
command -v schedulerd            → /usr/local/bin/schedulerd
ls -la /usr/local/bin/schedulerd → 20266468 bytes, -rwxr-xr-x root, Jul 19 09:50
go version -m /usr/local/bin/schedulerd
  → go1.26.5
  → path github.com/coding-herms/scheduler/cmd/schedulerd      <- pre-rename module path
  → mod  github.com/coding-herms/scheduler v1.0.1-0.20260719145003-c56721124655+dirty
  → CGO_ENABLED=1        (no vcs.revision / vcs.time stamping at all)
```

Four reasons this is a real finding rather than residue:

1. **It is what `PATH` resolves.** `schedulerd` unqualified is that binary. The
   unit and the canary unit both use absolute paths, but an operator or a script
   typing the bare name gets a Jul-19 build.
2. **It predates the entire release line.** The oldest tag is `v1.1.0`
   (`2026-09-03`); 30 tags now exist, and this build is `v1.0.1-0.20260719…+dirty`.
3. **It is not even the same build class.** Released assets are
   `CGO_ENABLED=0` static cross-compiles (`.github/workflows/release.yaml:49,52`);
   this one is `CGO_ENABLED=1`, so it links the host libc and can never be
   compared to, or substituted for, a shipped artifact.
4. **Its flag surface is from before the fleet hardening.** `strings` on it finds
   `namespace-mode` but **zero** occurrences of `no-exec-fallback` or
   `groups-file`. So the current unit `ExecStart` would die at flag parse — the
   benign case — but a bare `schedulerd -db <path>` starts a pre-rename daemon
   with four-month-old migration code.

Not covered by any existing row, checked rather than assumed: `GAP-040`
(complete, `2026-08-13`) names a **different** path — the outer checkout binary
`/home/kara/coding-hermes-scheduler/schedulerd` — and its acceptance
(`ls /home/kara/coding-hermes-scheduler/schedulerd` → absent) **still holds**:
verified this pass, `No such file or directory`. `SCHED-GAP-162` covers the live
daemon at the repo `bin/` path, which is a different surface and a different fix.
Nothing references `/usr/local/bin/schedulerd` (`grep -rl` over
`~/.hermes/scripts/`, the repo’s `scripts/`, and `/etc/systemd` → no hits), so it
is dead weight that shadows a live name.

Filed as `RELEASE-051`, `P3`, pending, with three executable arms. It is **not** a
cut blocker and does not change Decision 1.

### Open-row census (scope: every non-terminal row, `blocked` and `review` included)

`731 complete`, `4 blocked`, `87 pending` → **91 non-terminal** of 822 rows
(pass 6 recorded 90; the delta is `RELEASE-050`, filed by the pass-6 posture
sweep after that record was drafted). By priority: 6 `P0`, 31 `P1`, 40 `P2`,
10 `P3`, 3 `P4`, 1 `P5`. The six `P0`s are unchanged:
`SEC-002` (owner-gated credential rotation) plus
`SCHED-GAP-16956` / `16960` / `16962` / `16964` / `16966` (node-provisioning
program rows). None names a defect in shipped code, so none is a tree or cut
blocker.

### Rows this pass

- `RELEASE-049` — the canonical row for this release line; extended with a
  pass-7 addendum and the new artifact fingerprint. Not retitled, not reprioritised.
- `RELEASE-051` — **new**, P3 pending: the stale `/usr/local/bin/schedulerd`
  shadow binary documented above.
- `RELEASE-048` — unchanged and still pending; its premise still holds live
  (`GET /api/v1/projects` → `coding-hermes-scheduler-releng`
  `last_tick_status=failed`, `last_tick_completed=2026-11-07T13:25:22-05:00`).
  Not touched: different owner, different defect.
- `RELEASE-050` — unchanged and still pending (repo-admin action this lane
  cannot perform).
- No duplicate row was filed for the not-due release, and no existing row was
  closed in this pass.

## Pass 8 — eighth delivery, 49 minutes after the `v1.15.1` cut: NOT DUE, the `RELEASE-048` fix is observed mid-flight in the canonical checkout, artifact re-downloaded and re-verified from published bytes

Probes began `2026-11-07T22:03Z`. Every command in this pass ran in the detached
worktree `/var/tmp/hermes-scratch/releng-sched-p8-54z1tt`, created with
`git worktree add --detach` at the fetched `origin/main`. Its root plus remote
were asserted before use (`/var/tmp/hermes-scratch/releng-sched-p8-54z1tt`,
`https://github.com/coding-hermes/scheduler.git`) and it is clean (0 porcelain
lines).

### Re-anchor — three views agree at `88e78618`

```
git rev-parse HEAD                                → 88e786186d272068cdddcf56019bca1a92daa965
git ls-remote origin refs/heads/main              → 88e786186d272068cdddcf56019bca1a92daa965
gh api repos/coding-hermes/scheduler/commits/main → 88e786186d272068cdddcf56019bca1a92daa965
git ls-remote --symref origin HEAD                → ref: refs/heads/main
```

The tip moved four times inside this tick's lifetime (`c6cc9920` at pass 7 →
`88e78618` now), which is why this pass re-derived the census from the fetched
remote tip rather than inheriting pass 7's numbers.

### Canonical checkout — an ACTIVE sibling writer, and the work it is holding is `RELEASE-048` itself

`git rev-list --left-right --count origin/main...HEAD` → `10  0`: the checkout is
still 10 commits behind, now carrying **16 modified + 4 untracked paths**. The
mtimes are `16:34`–`16:55`, i.e. all *after* the `16:13` cut, and a session
process is visibly polling that tree:

```
1784137  277  ... eval 'sleep 330; cd /home/kara/coding-hermes-scheduler/coding-herms-scheduler && ...
              git log --oneline -2; git status --porcelain --untracked-files=all | wc -l'
```

So this is a live writer, not abandoned residue. The paths identify what it is
building — the `RELEASE-048` repair itself:

```
internal/scheduler/tick_reconcile.go   "// tick_reconcile.go — RELEASE-048: late-commit reconciliation
                                        // for failed and timeout ticks."
internal/scheduler/tick_reconcile_test.go
cmd/reconcile-ticks/main.go            (new CLI)
internal/scheduler/{lifecycle,tick_process}.go, internal/database/migrations.go
scripts/release-claim.sh, Makefile, docs/releases.md, CHANGELOG.md, .gitignore
```

No release action was attempted from that tree and no file in it was touched by
this pass. It is **not** release content: none of it is committed, so
`v1.15.1..origin/main` does not reach it. The stale-base hazard `RELEASE-046`
described is guarded now (`d2b961a0`, "refuse a stale-base board commit before
it can drop rows"), so no row is filed for the dirty tree — the work is being
written, not abandoned.

### Delta since the pass-7 tip `c6cc9920` — bookkeeping only

```
git diff --name-only c6cc9920..88e78618
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

Zero code paths again.

### Census `v1.15.1..88e78618` — reconciles

```
git rev-list --no-merges --count v1.15.1..HEAD                       → 13
git log --format='%s' v1.15.1..HEAD | sed -E 's/^([a-z]+)(\(.*\))?!?:.*/\1/' | sort | uniq -c
      9 board
      4 chore
git log --format='%B' v1.15.1..HEAD | grep -cE 'BREAKING CHANGE|^[a-z]+(\(.*\))?!:'   → 0
git diff --name-only v1.15.1..HEAD
.coding-hermes/board/board.jsonl
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
.gitreins/tasks.yaml
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

`9 + 4 = 13` equals `rev-list`, so the census reconciles. Every subject is a
`board(releng)` / `chore(releng)` bookkeeping commit carrying `[ci skip]`, and the
path list touches no shipped source. Material count: **0 feat / 0 fix / 0 perf /
0 breaking**.

### Decision 1 — NOT DUE (rule 3)

`v1.15.1` published `2026-11-07T21:13:45Z`, 49 minutes before this pass's first
probe. Zero material commits have landed since. Rule 3 fires ahead of rule 6's
14-day cadence and ahead of every bump. No tag, Release object, or asset was
created, moved, or deleted — and no tag can be: `v1.15.1` is the newest tag both
locally and in `ls-remote`.

### CI — the tip has no run by construction; the tree gate is carried by `f6ea23d7`

```
gh api "repos/.../actions/runs?head_sha=88e786186d272068cdddcf56019bca1a92daa965" --jq .total_count → 0
git diff --name-only f6ea23d7..HEAD | grep -vE '^(\.coding-hermes/|\.gitreins/|docs/|CHANGELOG)'  → no output, rc=1
```

`88e78618` carries `[ci skip]` and a board/evidence-only diff, so no run exists
for it and none can. The shipped code tree is **byte-identical** to `f6ea23d7`
(the filter above removes only board, `.gitreins`, docs and CHANGELOG paths), so
the release gate is the `f6ea23d7` measurement, re-read this pass:

| Run | Workflow | SHA | Result |
|---|---|---|---|
| 42412373587 | `CI` | `f6ea23d7` | completed/success — 14 jobs, re-censused per job this pass: `lint`, `build`, `race-blocks`, `race-scheduler`, `multiarch / Verify (amd64)`, `multiarch / Verify (arm64, native)` and all six `multiarch / Build <platform>` success; `Container (multi-arch)` + `Release` `skipped` (tag-only `if:`) |
| 42412372572 | `CI Pipeline` | `f6ea23d7` | completed/success — 7/7 |
| 42414729229 | `Release` (tag) | `f6ea23d7` | completed/success |
| 42414729246 | `CI Pipeline` (tag) | `f6ea23d7` | completed/success — 7/7 |

### Version hygiene at the tip — PASS

```
bash scripts/check-version-authority.sh v1.15.1
→ version-authority: PASS — .../CHANGELOG.md names v1.15.1; [Unreleased] present exactly once
grep -nE '^## \[[0-9]' CHANGELOG.md → 36:## [1.15.1] — 2026-11-07
                                     48:## [1.15.0] — 2026-11-07
```

The `[Unreleased]` heading sits above `[1.15.1]` and is empty — correct post-release
rollover.

### Shipped artifact `v1.15.1` — re-downloaded from the Release and re-verified

Pass 7 relied on immutability; this pass re-fetched the bytes and repeated every
check, so the receipt is fresh rather than inherited:

```
gh release view v1.15.1 --json tagName,isDraft,isPrerelease,assets
→ tag v1.15.1, draft=false, prerelease=false, 9 assets
gh release download v1.15.1 -D <dir>  → 9 files, 106 MB total
(cd <dir> && sha256sum -c SHA256SUMS) → 8/8 OK
```

Provenance, all eight binaries (not a sample):

```
schedulerd-{linux,darwin}-{amd64,arm64}  vcs.modified=false  mod … v1.15.1
migrate-{linux,darwin}-{amd64,arm64}     vcs.modified=false  mod … v1.15.1
schedulerd-linux-amd64 / migrate-linux-amd64: go1.26.10, vcs.revision=f6ea23d757daab8eef1d04f5c6c4f798659d504b
```

### Deployed binary — still STALE, still two releases behind

- pid `3347652`, started `Sat Nov 7 00:42:57 2026`, elapsed `16:20:13` at probe.
- Executable `…/coding-herms-scheduler/bin/schedulerd`, mtime `2026-11-07 00:42:57`
  — the unit's own `ExecStart` path, rebuilt by its `ExecStartPre` on restart.
- `go version -m` → `mod github.com/coding-hermes/scheduler
  v1.14.1-0.20261107042958-d0dad6d9ede0+dirty`, `vcs.revision=d0dad6d9ede0…`,
  `vcs.modified=true`.

The live scheduler is missing **`v1.15.0` and `v1.15.1`** and runs a `+dirty`
build — identical to passes 6 and 7, owned by `SCHED-GAP-162` (pending/P1) with
its recurrence row `SCHED-GAP-16994` (complete). Not duplicated here; it does not
block a cut because no cut is due.

### Open-row census at `88e78618` (scope: every non-terminal row, `blocked` and `review` included)

`731 complete`, `88 pending`, `4 blocked` → **92 non-terminal of 823 rows**
(`731 + 88 + 4 = 823`, equals the line count). By priority: 6 `P0`, 31 `P1`,
40 `P2`, 11 `P3`, 3 `P4`, 1 `P5`.

Delta against pass 7 (91 of 822): **+1 row, +1 `P3`** — `RELEASE-051`, which pass 7
filed *after* its own census was drafted and therefore could not count. No row was
closed by a sibling in between. The six `P0`s are unchanged and none names shipped
code: `SEC-002` (owner-gated credential rotation) plus
`SCHED-GAP-16956` / `16960` / `16962` / `16964` / `16966` (node-provisioning
program rows), so none is a tree or cut blocker.

### `RELEASE-048` premise re-probed — still live, and now visibly being fixed

```
GET http://127.0.0.1:9090/api/v1/projects → coding-hermes-scheduler-releng
  last_tick_status    = "failed"
  last_tick_completed = 2026-11-07T16:22:50-05:00
  enabled             = true, last_error = null
```

The ledger still books the releng lane as failed for the tick it is executing, and
the timestamp has advanced from pass 7's `13:25` to `16:22:50` — every delivery of
this tick re-writes it as a failure. The row's premise holds, and the in-flight
`internal/scheduler/tick_reconcile.go` shown above is that row's repair being
written. Row left untouched: different owner, different defect, work in progress.

### Rows this pass

- `RELEASE-049` — the canonical row for this release line; evidence entry appended
  with this pass's artifact fingerprint and the tick id as the run leg. Not
  retitled, not reprioritised.
- **No new row filed.** Every candidate finding is already owned: stale deployed
  binary → `SCHED-GAP-162`; stale `/usr/local/bin/schedulerd` shadow →
  `RELEASE-051`; disabled secret scanning + push protection → `RELEASE-050`;
  ledger invisibility + in-flight repair → `RELEASE-048`; dirty canonical
  checkout → live writer, guarded by `d2b961a0`.
- No row was closed and no existing row was modified beyond `RELEASE-049`'s
  evidence entry.

## Pass 9 — ninth delivery, ~1 h 14 m after the `v1.15.1` cut: NOT DUE (rule 3), published artifact still byte-clean, one carried blocker re-proved live and its stale title corrected

Probe time 2026-11-07T22:25Z. Tick `coding-hermes-scheduler-releng-2026-11-07-15-25-21`, delivery 9.
Same tick id as passes 1-8; per the re-delivery rule this pass EXTENDS this one artifact rather than
creating a second record of one decision.

### Re-anchor

| Probe | Value |
| --- | --- |
| Canonical repo | `/home/kara/coding-hermes-scheduler/coding-herms-scheduler` |
| Remote | `origin https://github.com/coding-hermes/scheduler.git` (from `git remote -v`) |
| Default branch | `main` |
| `git fetch origin` → `git rev-parse origin/main` | `7efe4c39a38237bbe335fb9bd069f25477de3874` |
| `git ls-remote origin refs/heads/main` | `7efe4c39a38237bbe335fb9bd069f25477de3874` |
| Detached worktree HEAD | `7efe4c39a38237bbe335fb9bd069f25477de3874`, `git status --porcelain` = 0 lines |
| Last release | `v1.15.1` → tag object `e8ae6455`, commit `f6ea23d757daab8eef1d04f5c6c4f798659d504b` (annotated; remote `^{}` agrees) |
| Release published | `2026-11-07T21:12:10Z` (`gh release view v1.15.1`), `isDraft=false`, `isPrerelease=false`, 9 assets |

All probes below ran inside the detached worktree at `7efe4c39`; the canonical checkout was NOT used
(it is held by an active sibling writer, see "Observed" below).

### Delta since pass 8, and the full census rather than an inherited one

Pass 8 probed `88e78618`; this pass probes `7efe4c39`.

```
$ git diff --name-only 88e78618..7efe4c39
.coding-hermes/board/tasks.jsonl
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

Two commits, both releng bookkeeping: `7773fa85` (pass-8 record) and `7efe4c39` (pass-8 board entry).
Both carry a `[ci skip]` directive in the message body.

Full range re-derived, not counted forward:

```
$ git rev-list --count v1.15.1..7efe4c39
15
$ git log --format='%s' v1.15.1..7efe4c39 | awk '{ ...conventional prefix census... }'
feat=0 fix=0 perf=0 deps=0 other-conv=5 unconv=10
$ git log --format='%h %s%n%b' v1.15.1..7efe4c39 | grep -c 'BREAKING CHANGE'
0
$ git diff --name-only v1.15.1..7efe4c39
.coding-hermes/board/board.jsonl
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
.gitreins/tasks.yaml
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

15 = 13 (pass-8's census, unchanged) + the 2 pass-8 bookkeeping commits; the 5 `other-conv` are
`board:`/`chore(releng):` prefixes and the 10 "unconventional" are the earlier `board:` subjects of the
same bookkeeping family. Zero material commits, zero BREAKING, zero product paths. The parser is not
silently empty: the classification sums to 15 and the path diff independently confirms bookkeeping-only.

### Decision

NOT DUE — **rule 3** (zero material commits since the last release). Rule 6 is also unmet (0 days, 0
material commits vs the 14-day / 5-material threshold). `v1.15.1` was published 1 h 14 m before this
probe; there is nothing to cut.

### Exact-SHA CI

```
$ gh api "repos/coding-hermes/scheduler/actions/runs?head_sha=7efe4c39a38237bbe335fb9bd069f25477de3874" --jq '.total_count'
0
$ gh api "repos/coding-hermes/scheduler/actions/runs?head_sha=7773fa85..." --jq '.total_count'
0
```

Absent by construction, and explained from the trigger surface rather than assumed: `.github/workflows/ci.yml`
and `ci.yaml` both trigger on `push: branches: [main]` with no `paths:` filter, so the reason no run exists
is the skip directive every commit in the range carries (`grep -c 'ci skip'` over the 15 subjects = 14, the
fifteenth carries it in the body), not a path filter. The shipped code tree is byte-identical to the
certified code SHA:

```
$ git diff --name-only f6ea23d7..7efe4c39      # only board, .gitreins and docs/release-evidence
```

Receipts on `f6ea23d7`, all terminal:

| Run | Workflow | Trigger | Result | Jobs |
| --- | --- | --- | --- | --- |
| 42412373587 | CI | push | completed/success | 12 executed (2 tag-only jobs skipped) |
| 42412372572 | CI Pipeline | push | completed/success | 7 |
| 42414729229 | Release | tag `v1.15.1` | completed/success | 1 (Release) |
| 42414729246 | CI Pipeline | tag `v1.15.1` | completed/success | 7 — Race Detector (internal/api), Test (Go 1.26), Lint, Build Binaries, Simulation Smoke Test, Integration Test, CI All Clear |

The candidate `7efe4c39` has no run of its own; the run that DECIDES the tree gate is the code-equivalent
`f6ea23d7` arm above, and the byte-identity is quoted rather than asserted.

### Gates

- Version authority (structure): `bash scripts/check-version-authority.sh --structure-only` → `version-authority: PASS (structure) — exactly one [Unreleased]; newest release heading '## [1.15.1] — 2026-11-07'` (rc=0). The `[Unreleased]` section is empty, which is correct for a tree with no material delta.
- GitReins: **NOT RUN, by construction** — the range has zero material commits, so a candidate-bound full-tree guard on a pristine tree returns `changedFileCount: 0` and a DEGRADED verdict that is non-evidence. No `.gitreins/history/2026-11-*` directory exists; `.gitreins/tasks.yaml` still shows the pre-cut RELEASE-044/045/046 task records in `in_progress` while their board rows are complete, which is the same ledger-vs-board divergence `RELEASE-048` tracks, not a new finding.
- Dogfood: no `.coding-hermes/dogfood*` cell record for `v1.15.1`. Checked before filing anything: `docs/releases.md` contains **zero** mentions of dogfood and `scripts/release-prep.sh` has no dogfood step, so dogfood is not a documented release gate on this repo and the absence is not a gate miss. No row filed.
- Dependencies: `go list -u -m all | grep -c '\['` → **0** modules with an available update; `govulncheck ./...` → `No vulnerabilities found.` Dependency policy: nothing to move.

### Published artifact, re-verified from published bytes

```
$ gh release download v1.15.1 -D <scratch> --clobber     # 9 files
$ sha256sum -c SHA256SUMS
./migrate-darwin-amd64: OK ... ./schedulerd-linux-arm64: OK     (8/8 OK)
$ go version -m <each of 8 binaries> | grep -E 'mod\s|vcs\.'
mod github.com/coding-hermes/scheduler v1.15.1   vcs.revision=f6ea23d7…   vcs.modified=false   (ok=8 bad=0)
```

### Deployed artifact (separate finding, still live)

```
$ curl -s http://127.0.0.1:9090/api/v1/health | jq -r .build_sha,.build_time
d0dad6d9
2026-11-07T05:42:56Z
$ systemctl --user list-timers --all | grep -c sched-deploy
0
$ systemctl --user show coding-hermes-scheduler -p ExecMainStartTimestamp
ExecMainStartTimestamp=Sat 2026-11-07 00:42:58 -05   (binary mtime 00:42:57.143 → no build-without-restart)
```

The live daemon is `v1.14.1-0.20261107042958+dirty` (`d0dad6d9`, built 2026-11-07T04:29:58Z, started
05:42:57Z): **two published releases behind** (`v1.15.0`, `v1.15.1`), with no deploy timer armed. This is
`SCHED-GAP-162`'s subject and it stays P1 `pending` — this pass **confirms** the row's 20:00Z measurement
rather than contradicting it, because every commit since `72c4b216` is board/releng bookkeeping and the code
gap is therefore unchanged. What this pass DID repair is the row's title, which still advertised a
`v1.12.0-684-gc1f2f033` build "10 commits / 6 code" behind — three releases stale. Corrected through the
writer to the measured figures, with the probe output appended as the row's note. A reader scanning titles
is no longer pointed at a July build.

### Posture and PATH shadow, both still live (carried, re-proved)

```
$ gh api repos/coding-hermes/scheduler --jq '.security_and_analysis'
{"secret_scanning":{"status":"disabled"},"secret_scanning_push_protection":{"status":"disabled"}}
$ gh api repos/coding-hermes/scheduler/branches/main/protection     # no required_status_checks emitted
$ command -v schedulerd ; ls -la /usr/local/bin/schedulerd
/usr/local/bin/schedulerd  root  2026-07-19  20266468 bytes
$ go version -m /usr/local/bin/schedulerd | grep -E 'mod\s|vcs\.'
mod github.com/coding-herms/scheduler v1.0.1-0.20260719145003-c56721124655+dirty
vcs.revision=c56721124655…  vcs.modified=true
```

`RELEASE-050` (secret scanning + push protection disabled on a public repo publishing 8 binaries per
Release) and `RELEASE-051` (stale Jul-19 binary shadowing the canonical build on `PATH`, pre-rename module
path, CGO-linked) are both still open and both still true. Neither blocks a cut; neither has an owner-side
move yet. No new row filed for either — re-filing the same finding is the duplicate the board already
carries.

### Observed: the canonical checkout is still an ACTIVE sibling writer

The canonical checkout remains held by the `RELEASE-048` worker, ~1 h after dispatch
(`/home/kara/.hermes/cache/scratch/tick-2026-11-07-21-16-12/worker-RELEASE-048.log`, last write 22:10Z;
polling shells alive in `ps`):

```
$ git -C <canonical> status --porcelain   # 13 files, +1562/−7 staged
M  Makefile  M  README.md  A cmd/reconcile-ticks/main.go  M docs/releases.md
M  internal/database/migrations.go  M internal/scheduler/{adaptive_cooldown,lifecycle,tick_process}.go
A  internal/scheduler/tick_reconcile.go (+442)  A tick_reconcile_test.go (+636)
A  scripts/release-claim.sh (+175)   M CHANGELOG.md
```

That delta is uncommitted, therefore **not release content**, and no release action was attempted from it.
It also means `make tag` is about to gain a single-writer claim (`scripts/release-claim.sh`); when it lands,
the next cut uses the repo's own driver through that claim rather than tagging directly. Recorded so the
next pass does not mistake the new driver for a regression or ignore it as unrelated.

### Rows

- `RELEASE-049` (the canonical survivor for the `v1.15.1` line) — evidence entry appended for this pass; status, title and priority untouched (the cut it records is still complete and still true).
- `SCHED-GAP-162` — title corrected, probe note appended. Status untouched.
- **No new RELEASE row filed**: the range has no material delta, and all three open release-line rows (`RELEASE-048`, `RELEASE-050`, `RELEASE-051`) already own their findings. Filing a fourth parallel row for an unchanged state is the duplicate this pass exists to avoid.

Open non-terminal rows this pass did NOT touch (scope: every `pending|blocked|in_progress|review` row on the board; 88 pending + 4 blocked at probe time): the P0/P1 set includes `SEC-002`, `SCHED-GAP-16929` (host disk), `SCHED-GAP-16956/16960/16962/16964/16966` (node/bunker ladder), `INT-CI-191` (race) and `SCHED-GAP-162` (deploy gap). None is a release blocker for a NOT-DUE decision; each is its own owner's contract.

## Pass 10 — tenth delivery, 1 h 09 m after the `v1.15.1` cut: NOT DUE (rule 3), published bytes re-verified from the Release, and the posture census widened to the full disabled-switch set

Probes began `2026-11-07T22:20Z`. Tick `coding-hermes-scheduler-releng-2026-11-07-15-25-21`,
delivery 10 — same tick id as passes 1-9, so per the re-delivery rule this pass
EXTENDS this one artifact rather than creating a second record of one decision.

### Re-anchor — three views agree at `08ef9e2b`

| Probe | Value |
| --- | --- |
| Canonical repo | `/home/kara/coding-hermes-scheduler/coding-herms-scheduler` |
| Remote (verified alias `origin`) | `https://github.com/coding-hermes/scheduler.git` |
| Default branch | `main` (`git ls-remote --symref origin HEAD` → `ref: refs/heads/main`) |
| `git rev-parse origin/main` | `08ef9e2b2376093c02f86f3490aaa9b07a046f10` |
| `git ls-remote origin refs/heads/main` | `08ef9e2b2376093c02f86f3490aaa9b07a046f10` |
| Provider `commits/main` | `08ef9e2b2376093c02f86f3490aaa9b07a046f10`, committed `2026-11-07T22:18:13Z` |
| Detached worktree | `/home/kara/.hermes/cache/scratch/releng-wt-p10-08ef9e2`, HEAD = the same SHA, `git status --porcelain` = 0 lines |
| Last release | `v1.15.1` → tag object `e8ae6455`, commit `f6ea23d757daab8eef1d04f5c6c4f798659d504b`; remote `refs/tags/v1.15.1^{}` = the same commit |
| Release object | `tag=v1.15.1 draft=false prerelease=false published=2026-11-07T21:13:45Z assets=9` |

`git ls-remote --tags --refs` newest five by version sort: `v1.13.0`, `v1.14.0`, `v1.14.1`,
`v1.15.0`, `v1.15.1` — no tag exists above `v1.15.1`, locally or remotely, so there is
nothing to move and nothing to preserve.

Every probe below ran inside the detached worktree; its root and remote were asserted
before use. The canonical checkout was NOT used (still held by an active sibling writer,
see below).

### Delta since the pass-9 tip `7efe4c39` — bookkeeping only

```
$ git diff --name-only 7efe4c39..08ef9e2b
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

Two commits, both releng bookkeeping: `0b291d6a` (pass-9 record) and `08ef9e2b` (pass-9
board entry + `SCHED-GAP-162` retitle). Zero code paths.

### Census `v1.15.1..08ef9e2b` — reconciles, re-derived rather than counted forward

```
$ git rev-list --count v1.15.1..HEAD → 17
$ git log --format='%s' v1.15.1..HEAD | sed -E 's/^([a-z]+)(\(.*\))?!?:.*/\1/' | sort | uniq -c
     11 board
      6 chore
$ git log --format='%B' v1.15.1..HEAD | grep -cE 'BREAKING CHANGE|^[a-z]+(\(.*\))?!:'  → 0
$ git diff --name-only v1.15.1..HEAD
.coding-hermes/board/board.jsonl
.coding-hermes/board/events.jsonl
.coding-hermes/board/tasks.jsonl
.gitreins/tasks.yaml
docs/release-evidence/2026-11-07-coding-hermes-scheduler-releng-2026-11-07-15-25-21.md
```

`11 + 6 = 17` equals `rev-list`, so the classification covers the whole range and is not a
silently-empty parser. Material count: **0 feat / 0 fix / 0 perf / 0 deps / 0 breaking**.
Every commit is `board(releng)` / `chore(releng)` traffic carrying a `[ci skip]` directive
(22 directive hits across subjects+bodies of the 17 commits), and the independent path
diff touches no shipped source.

### Decision 1 — NOT DUE (rule 3)

`v1.15.1` published `2026-11-07T21:13:45Z`, 1 h 09 m before this pass's first probe.
Rule 3 (zero material commits since the last release) fires ahead of rule 6's 14-day
cadence and ahead of every bump; the range carries no material commit to bump on. No tag,
Release object, or asset was created, moved, or deleted in this pass.

### Exact-SHA CI — the tip has no run by construction; the tree gate is carried by `f6ea23d7`

```
$ gh api "repos/coding-hermes/scheduler/actions/runs?head_sha=08ef9e2b2376093c02f86f3490aaa9b07a046f10" --jq .total_count → 0
$ gh api "repos/coding-hermes/scheduler/actions/runs?head_sha=0b291d6a…" --jq .total_count                                    → 0
$ gh api "repos/coding-hermes/scheduler/actions/runs?head_sha=7efe4c39…" --jq .total_count                                    → 0
$ git diff --name-only f6ea23d7..HEAD | grep -vE '^(\.coding-hermes/|\.gitreins/|docs/|CHANGELOG)'  → no output, rc=1
```

Absent by construction, and explained from the trigger surface rather than assumed:
`.github/workflows/ci.yml` and `ci.yaml` trigger on `push: branches: [main]` with no
`paths:` filter, so the reason no run exists is the `[ci skip]` directive every commit in
the range carries — not a path filter. The shipped code tree is **byte-identical** to
`f6ea23d7`: the filtered diff above removes only board, `.gitreins`, `docs/` and
`CHANGELOG` paths and returns nothing, so the decision leg is the code-equivalent
certified SHA, whose receipts were re-read this pass and are all terminal:

| Run | Workflow | Trigger | SHA | Result |
| --- | --- | --- | --- | --- |
| 42412373587 | `CI` | push | `f6ea23d7` | completed/success — 12 executed jobs (2 tag-only jobs `skipped`) |
| 42412372572 | `CI Pipeline` | push | `f6ea23d7` | completed/success — 7/7 |
| 42414729229 | `Release` | tag `v1.15.1` | `f6ea23d7` | completed/success |
| 42414729246 | `CI Pipeline` | tag `v1.15.1` | `f6ea23d7` | completed/success — 7/7 |

The newest run in the repository is still `42414729246` (created `21:12:13Z`), which
confirms that no push since the tag produced a run — consistent with the skip directives
rather than with a queued or missing workflow.

### Gates

- Version authority (structure): `bash scripts/check-version-authority.sh --structure-only`
  → `version-authority: PASS (structure) — exactly one [Unreleased]; newest release heading
  '## [1.15.1] — 2026-11-07'` (rc=0). Headings: `34:## [Unreleased] — 2026-11-07`,
  `36:## [1.15.1] — 2026-11-07`, then `[1.15.0]`, `[1.14.1]`, `[1.14.0]`, `[1.13.0]`.
  `[Unreleased]` is empty and sits above the newest release heading — the correct
  post-release rollover for a tree with no material delta. `internal/version/version.go:26`
  is still `var Version = "dev"` (ldflags-injected at build time), so the stamp that counts
  is the shipped binaries' `mod … v1.15.1`, verified below.
- GitReins: **NOT RUN, by construction** — zero material commits in the range, so a
  candidate-bound full-tree guard on a pristine tree yields `changedFileCount: 0` and a
  DEGRADED verdict that is non-evidence. No `.gitreins/history/2026-11-*` directory exists
  (newest is `2026-09-19`).
- Dogfood: no cell record for `v1.15.1` and the log carries **0** mentions of `v1.15`;
  `docs/releases.md` and `scripts/release-prep.sh` contain **0** mentions of dogfood, so
  dogfood is not a documented release gate on this repo and the absence is an ABSENCE, not
  a skipped gate. No row filed.
- Dependencies: `go list -u -m all | grep -c '\['` → **0** modules with an available
  update; `govulncheck ./...` → `No vulnerabilities found.` Nothing to move, nothing
  blocked.

### Published artifact `v1.15.1` — re-downloaded and re-verified from the Release bytes

```
$ gh release download v1.15.1 --clobber -D <scratch>   → 9 files, 96 MB
$ sha256sum -c SHA256SUMS                              → 8/8 OK
   (migrate|schedulerd)-{darwin,linux}-{amd64,arm64}
$ go version -m <each of the 8 binaries>
   mod github.com/coding-hermes/scheduler v1.15.1
   vcs.revision=f6ea23d757daab8eef1d04f5c6c4f798659d504b
   vcs.modified=false                              → ok=8 bad=0
```

The revision stamped in the published bytes equals the tagged commit on all eight, and the
`+dirty` / `vcs.modified=true` class that `RELEASE-037` opened is absent from every asset.
The release object is `draft=false prerelease=false` with 9 assets, `v1.15.1` marked
`Latest`.

### Deployed artifact — unchanged, still two published releases behind (`SCHED-GAP-162`)

```
$ curl -s http://127.0.0.1:9090/api/v1/health | jq -r .build_sha,.build_time
d0dad6d9
2026-11-07T05:42:56Z
$ ps -o pid,lstart,etimes -p 3347652
  3347652  Sat Nov  7 00:42:57 2026   59983 (16h39m at probe)
$ stat -c '%y %s' …/coding-herms-scheduler/bin/schedulerd → 2026-11-07 00:42:57.143, 18,665,737 B
$ go version -m <that binary>
   mod github.com/coding-hermes/scheduler v1.14.1-0.20261107042958-d0dad6d9ede0+dirty
   vcs.revision=d0dad6d9ede03d15927ec5d249aee9c9ee41f73c   vcs.modified=true
```

The live daemon runs a `+dirty` build from before both releases and is missing **`v1.15.0`
and `v1.15.1`**. Binary mtime equals process start (`00:42:57.143` vs `00:42:58` unit
start), so this is a stale artifact, not a build-without-restart. Owned by `SCHED-GAP-162`
(P1, pending) — this pass **confirms** the pass-9 measurement rather than widening it,
because every commit since is board bookkeeping and the code gap is unchanged. Not
duplicated. It does not block a cut: no cut is due.

### Posture — the disabled-switch census is WIDER than `RELEASE-050` records

```
$ gh api repos/coding-hermes/scheduler --jq '{vis:.visibility, sa:.security_and_analysis}'
{"vis":"public","sa":{
  "dependabot_security_updates":          {"status":"disabled"},
  "secret_scanning":                      {"status":"disabled"},
  "secret_scanning_non_provider_patterns":{"status":"disabled"},
  "secret_scanning_push_protection":      {"status":"disabled"},
  "secret_scanning_validity_checks":      {"status":"disabled"}}}
```

`RELEASE-050` was filed (pass 6) on the two switches named in its title. The other three —
`dependabot_security_updates`, `secret_scanning_non_provider_patterns` and
`secret_scanning_validity_checks` — are the **same owner action on the same settings page**
and are all free on public repositories. Per the no-duplicate rule this is a **note appended
to `RELEASE-050`**, not a second row: one repo-admin visit clears all five, and splitting it
across two rows would give a foreman two candidates for one switch panel. Status, title and
acceptance of the row are untouched — the title's two named switches remain true.

`gh api repos/coding-hermes/scheduler/branches/main/protection` this pass returns an object
(`required_linear_history=true`, `allow_force_pushes=false`, `required_approving_review_count=1`,
`enforce_admins=false`) with **no `required_status_checks` key emitted**, i.e. no required
checks on `main`. That governance half stays on `REL-GUARD-001` as passes 6-9 decided; it is
not folded in here.

### PATH shadow — `RELEASE-051` still live, re-measured

```
$ command -v schedulerd            → /usr/local/bin/schedulerd
$ stat -c '%y %s' /usr/local/bin/schedulerd → 2026-07-19 09:50:33, 20,266,468 B
$ go version -m /usr/local/bin/schedulerd
   mod github.com/coding-herms/scheduler v1.0.1-0.20260719145003-c56721124655+dirty
   CGO_ENABLED=1
```

Pre-rename module path, CGO-linked, four months old, unreferenced by any unit — still
shadows the canonical build on `PATH`. Row unchanged, still pending, still true.

### Open-row census (scope: every non-terminal row, `blocked` and `review` included)

Board at `08ef9e2b` parses clean (`jq -s length` → **823** rows, 0 bad lines):

```
$ jq -r 'select(.status|test("^(pending|blocked|in_progress|review)$")) | .status' tasks.jsonl | sort | uniq -c
      4 blocked
     88 pending
$ … | .priority | uniq -c → 6 P0, 31 P1, 40 P2, 11 P3, 3 P4, 1 P5
```

**92 non-terminal of 823** — identical to pass 9's census (88+4, same priority split), so
no row was closed or filed by any lane between passes. The six `P0`s are unchanged:
`SEC-002` (owner-gated credential rotation) plus `SCHED-GAP-16956` / `16960` / `16962` /
`16964` / `16966` (node-provisioning program rows). None names a defect in shipped code, so
none is a tree or cut blocker.

### `RELEASE-048` premise — still live at `2026-11-07T22:22Z`

```
$ curl -s http://127.0.0.1:9090/api/v1/projects | jq -r '.projects[]|select(.name|test("^coding-hermes-scheduler-releng"))|"\(.name) \(.last_tick_status) \(.last_tick_completed)"'
coding-hermes-scheduler-releng failed 2026-11-07T16:22:50-05:00
```

The ledger still books this very lane as failed for the tick it is executing (the timestamp
has not advanced since pass 9's `16:22:50`), so the row's premise holds. Its repair is the
in-flight changeset described next. Row untouched: different owner, work in progress.

### Canonical checkout — still an ACTIVE sibling writer, and no release action was taken there

```
$ git rev-list --left-right --count origin/main...HEAD → 14  0
$ git status --porcelain --untracked-files=all | wc -l → 17
M .coding-hermes/board/{events,tasks}.jsonl   M .gitreins/tasks.yaml
M/A .gitignore, CHANGELOG.md, Makefile, README.md, docs/releases.md
A cmd/reconcile-ticks/main.go
M internal/database/migrations.go  M internal/scheduler/{adaptive_cooldown,lifecycle,tick_process}.go
A internal/scheduler/tick_reconcile.go  A internal/scheduler/tick_reconcile_test.go
A scripts/release-claim.sh   ?? reconcile-ticks
```

The `RELEASE-048` worker dispatched at `16:18` is **still running** at `17:22`
(pid `1405215`, elapsed 3838 s, log `/home/kara/.hermes/cache/scratch/tick-2026-11-07-21-16-12/worker-RELEASE-048.log`
last written `17:10`), and the paths carry that row's banner
(`// tick_reconcile.go — RELEASE-048: …`). This is a live writer, not abandoned residue, and
nothing in it is committed, so `v1.15.1..origin/main` does not reach it. No release action
was attempted from that tree; every probe above ran in the detached worktree at `08ef9e2b`.
Recorded for the next pass: when that changeset lands, `make tag` will route through the new
`scripts/release-claim.sh` single-writer claim, and that claim is not a regression.

### Rows this pass

- `RELEASE-049` — canonical survivor for the `v1.15.1` line; evidence entry appended with
  this pass's artifact fingerprint and the tick id as the run leg. Status, title, priority
  untouched (the cut it records is still complete and still true).
- `RELEASE-050` — **note appended**: the disabled-switch census widened from two switches to
  five, same owner action. No new row; status/title/acceptance untouched.
- **No new `RELEASE-*` row filed.** Every candidate finding is already owned: stale deployed
  binary → `SCHED-GAP-162`; stale `/usr/local/bin/schedulerd` → `RELEASE-051`; disabled
  scanning switches → `RELEASE-050`; releng-ledger invisibility + in-flight repair →
  `RELEASE-048`; dirty canonical checkout → live writer. A row for an unchanged, not-due
  state is the duplicate this pass exists to avoid.
- No row was closed, retitled or reprioritised in this pass.

Nothing in this pass rewrote a tag, deleted an asset, created a Release object, or touched
the `v1.15.0` / `v1.15.1` Release objects.
