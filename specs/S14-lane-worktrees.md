# S14 — Lane Worktrees (create, refresh, merge back, prune)

**Status:** Draft
**Depends on:** S13 (lane hooks), S12 (wave scheduling — for the boundary only), S02, S05
**Pages target:** 4-6

---

## 1. Overview

Every lane that writes to a repository needs its own checkout. Today most lanes run in a
`~/.hermes/stand-in/<role>/<project>` directory that is not a git repository at all: the lane
cannot read a board, cannot see the diff it is reviewing, and its only knowledge of the real
repo is a line of prompt text. Where a lane does run in a real tree, it shares the foreman's
tree and therefore its `index.lock`, its branch, and its uncommitted work.

S14 replaces both arrangements with one object: a **lane worktree** — a real `git worktree` of
the project repository at a fixed path, on a dedicated branch, refreshed before the tick and
merged back after it, all executed by S13 hooks.

> **L0 — One lane, one branch, one writer.** A lane's worktree path and branch are stable for
> the life of the lane. Two ticks of the same lane can never run at once (the lease forbids
> it), and the branch is never checked out anywhere else.

> **L1 — The lane's directory is the worktree.** `projects.workdir` points at the worktree, so
> the board read, the commit count, and the agent's cwd are all the same directory the hook
> prepared. No hook rewrites a path the scheduler has already read.

> **L2 — Nothing is deleted that was not proved landed.** A branch is pruned only after it is
> an ancestor of the base branch, or after an operator marks it abandoned. Unmerged work, and a
> dirty tree, always survive.

---

## 2. Object model

| Object | Path | Branch |
|---|---|---|
| project repository (the source of truth) | `projects.repo_url` → `local:/home/kara/<repo>` | base, usually `main` |
| lane worktree | `/home/kara/proj-wt-<lane>` | `lane/<project>/<role>` |
| wave worker worktree (S12, unchanged) | foreman-chosen | `wt/<taskid>` |

The two worktree kinds never share a branch namespace, so `git worktree prune` and a foreman's
recovery sweep cannot remove a lane's tree by pattern.

Lane role is the trailing role suffix (`-qa`, `-docs`, `-foreman`, …) or `primary` for a lane
whose name is the project itself (`code-factory` → `lane/code-factory/primary`). The mapping is
the same `family()`/suffix rule `lane_cadence.py` uses, implemented once in
`internal/lanehook/role.go`.

### 2.1 Lane fields

```go
type WorktreeRef struct {
	Repo     string `json:"repo"`      // absolute repo path, from repo_url
	Base     string `json:"base"`      // base branch, default "main"
	Path     string `json:"path"`      // == projects.workdir
	Branch   string `json:"branch"`    // lane/<project>/<role>
	Policy   string `json:"policy"`    // persistent | persistent-rebase | ephemeral
	Push     bool   `json:"push"`      // push the merge to origin after landing
	FloorGiB float64 `json:"floor_gib"`// admission floor for this checkout, default 5.0
}
```

Stored in `hooks.toml` under `[lane.<name>]` so it travels with the hook set:

```toml
[lane."code-factory-qa"]
repo     = "/home/kara/code-factory"
base     = "main"
policy   = "persistent"
push     = true
floor_gib = 8.0
```

`GET /api/v1/lanes/{name}/worktree` returns the resolved `WorktreeRef` plus live state
(`exists`, `dirty`, `ahead`, `behind`, `last_merge`).

---

## 3. The hook set

The default lane set, in the order S13 fires it. Priorities are the shipped values.

### 3.1 PRE (ascending)

| # | id | level | pri | on_failure | what it does |
|---|---|---|---|---|---|
| 1 | `lease-acquire` | global | 5 | closed | claim a `workspace_leases` row for (repo, branch, host, path); exit 75 if held |
| 2 | `repo-fetch` | global | 10 | closed | `git -C <repo> fetch --prune origin` with a 30 s cap |
| 3 | `board-sync` | namespace | 10 | closed | pull the shared board in the namespace's workdir; refuse a dirty board |
| 4 | `worktree-ensure` | lane | 20 | closed | create or refresh `<path>` on `<branch>` (§4) |
| 5 | `lane-bootstrap` | lane | 30 | open | install `.venv` / `node_modules` only when the marker file is absent |

### 3.2 POST (descending — first listed fires first)

| # | id | level | pri | on_failure | what it does |
|---|---|---|---|---|---|
| 1 | `quiesce-check` | lane | 95 | closed | prove no process holds the tree (§5); `closed` here stops the rest of POST |
| 2 | `lane-gate` | lane | 90 | closed | run the repo's declared gates on the branch (§6.2) |
| 3 | `lane-merge` | lane | 80 | closed | rebase → land → assert (§6) |
| 4 | `evidence-sweep` | namespace | 30 | open | move audit artefacts to `.coding-hermes/evidence/<lane>/` |
| 5 | `worktree-prune` | lane | 20 | open | remove the tree when `policy=ephemeral` or the branch is landed |
| 6 | `lease-release` | global | 5 | open | release the lease, `lease_epoch++` |

`on_failure = "closed"` in POST means **abort the remaining POST steps** (S13 §2.3), not
"fail the tick": a failed gate must not be followed by a merge, and a failed merge must not be
followed by a prune.

---

## 4. `worktree-ensure` — create or refresh

Contract, in order:

1. **Disk floor.** If free space on the worktree's filesystem `< floor_gib` → exit `78`
   (`EX_CONFIG`) with the measured number on stderr. Nothing is created.
2. **Repo sanity.** `<repo>` must be a git work tree and `origin/<base>` must resolve → else
   `65` (`EX_DATAERR`).
3. **Path state:**
   - *path is the repo's own primary worktree* → exit `10` with
     `reason=workdir-is-primary-checkout` (runner records the step as
     `state=skipped`, never a veto); never refresh, reset or clean the primary
     tree. The comparison resolves symlinks on both sides (`pwd -P`) and also
     matches the `--repo` path itself — a lane whose workdir equals the
     primary checkout must never require it to change branch (SCHED-GAP-16991).
   - *path absent* → `git -C <repo> worktree add <path> -B <branch> origin/<base>`
   - *path present, registered worktree of `<repo>`, on `<branch>`* → refresh (step 5)
   - *path present, on another branch* → `65`, do not touch the tree
   - *path present, not a registered worktree* → `git worktree prune`, then `worktree add`
     (a stale directory from a crashed host is the normal case and must not need a human)
4. **Branch in use elsewhere.** If `<branch>` is checked out in any other worktree →
   exit `75` (`EX_TEMPFAIL`) — a sibling tick owns it. `75` is retryable; S13's `retry:2`
   covers it.
5. **Refresh, per `policy`:**
   - `persistent` (default): if the tree is **dirty**, preserve it — write
     `dirty_preserved` to stdout and do **not** reset (a crashed tick's work is not garbage).
     If clean, `git -C <path> reset --hard origin/<base>` and `git clean -fd -e .venv -e node_modules -e target`.
   - `persistent-rebase`: rebase this branch's own commits onto `origin/<base>`; on conflict
     exit `65` with the conflicting paths and leave the branch untouched (`rebase --abort`).
   - `ephemeral`: hard reset to `origin/<base>` regardless of dirtiness (the lane is defined as
     disposable) — the only policy where dirty work is discarded, and it must be opted into.
6. Print one line to stdout: `worktree=<path> branch=<branch> base=<sha> ahead=<n> dirty=<0|1>`.
   That line is the receipt the closeout quotes.

Idempotency: running twice in a row must change nothing and print the same branch/base line.

---

## 5. `quiesce-check` — prove the tree is idle

A tree is quiescent when **no process** has its cwd, an open file, or an execution mapping
inside `<path>`.

```bash
# every pid whose cwd or fd resolves under the worktree
for p in /proc/[0-9]*; do
  [ "$p" = "/proc/$$" ] && continue
  readlink "$p/cwd" 2>/dev/null | grep -q "^$WT" && echo "$p cwd"
  ls -l "$p/fd" 2>/dev/null | grep -q " $WT/" && echo "$p fd"
done
```

- Exit `0` quiescent, `10` busy (prints the offending pids), `65` unreadable `/proc`.
- The runner's own pid, its process group, and the step's own children are excluded.
- `on_failure = "closed"`: a busy tree stops the POST list, so nothing merges and nothing is
  pruned. This is the guard that keeps a killed bunker exec (CF-204) or a stray harness from
  having its directory deleted underneath it.

---

## 6. `lane-gate` and `lane-merge` — land the work

### 6.1 Gate

`lane-gate` runs the repo's declared gate commands, in order, from `<path>`:

```toml
[lane."code-factory-qa".gates]
commands = [["make", "selftest"], ["gitreins", "guard", "run"]]
timeout_s = 900
```

Exit `0` = the branch may land. Any non-zero = the branch is preserved, `merge=blocked`,
event `hook.post.fail` with the failing command. A red tree is never merged.

### 6.2 Merge algorithm (exact)

```
dirty?                    → exit 65, merge=blocked, branch preserved        (never stash)
ahead == 0                → exit 10, merge=no_commits, branch preserved     (never "landed")
fetch origin              → fail = 65
behind > 0                → git rebase origin/<base>; conflict = 65 + rebase --abort
                            (rebase success rewrites the branch; that is the only rewrite allowed)
git checkout <base>       → in <repo>, never in <path>
git merge --ff-only <branch>
    conflict / not ff     → 65, merge=conflict, branch preserved, receipt has the diff stat
push == true              → git push origin <base>, fail = 75 (retryable, never --force)
```

Then **assert** (a merge that cannot prove itself is a failed merge):

```bash
git -C <repo> merge-base --is-ancestor <branch> origin/<base>   || exit 65
for f in $(git -C <path> diff --name-only origin/<base>...<branch>); do
  git -C <repo> cat-file -e "origin/<base>:$f" || exit 65        # file is in the landed tree
done
git -C <repo> log -1 --format='%H %s' origin/<base>
```

Exit codes: `0` landed · `10` nothing to do · `65` blocked/conflict (branch preserved) ·
`75` push contention (retryable, merge already local). The receipt always carries
`sha`, `files`, `landed=<0|1>`, and the assertion's answer — never a claim that a merge
happened without the ancestry proof.

### 6.3 Zero-commit branches

The fleet has been bitten by "landed" reports for branches with no commits
(`merge-base --is-ancestor` is trivially true). S14 forbids it at the source: `ahead == 0`
exits `10` before any git operation, `merge=no_commits`, and `worktree-prune` will not remove
a branch whose `merge` is not `merged`.

---

## 7. `worktree-prune`

Removes the tree when either `policy = "ephemeral"` or the branch is proven landed, and only
after `quiesce-check` returned 0 in the same POST pass.

```bash
git -C <repo> worktree remove --force <path>     # the tree, not the branch
git -C <repo> branch -d <branch>                 # only if merged; -d refuses otherwise
```

- `-d` never `-D`: git is the second lock on "was it landed".
- A branch whose tip is not an ancestor of `<base>` is kept, with the reason on stdout.
- The tree's `.venv`/`node_modules` are removed with the tree; that is why `policy` matters
  (persistent trees are refreshed, not pruned, on every tick).

---

## 8. Wiring

### 8.1 Moving a lane onto a worktree

Operator action, both stores (the scheduler re-pins `workdir` from `fleet.toml` at boot):

```bash
# 1. prepare the tree once, out of band
/home/kara/.hermes/hooks/worktree-ensure.sh --lane code-factory-qa --repo /home/kara/code-factory \
    --worktree /home/kara/proj-wt-code-factory-qa

# 2. re-point the lane (API), then mirror to fleet.toml
curl -s -X PUT -H "X-Operator-Token: $TOKEN" -H 'Content-Type: application/json' \
  -d '{"workdir":"/home/kara/proj-wt-code-factory-qa"}' \
  http://127.0.0.1:9090/api/v1/projects/code-factory-qa
python3 ~/.hermes/scripts/fleet-sync.py --write
```

Acceptance for the move: `GET /api/v1/projects/code-factory-qa` shows the new `workdir`;
`git -C <workdir> rev-parse --show-toplevel` equals the worktree; the next tick's
`hook_runs` shows `worktree-ensure` at `state=ok`; `ticks.commits` is counted from the
worktree (assert one commit in a scratch run).

### 8.2 Files

| Path | Role |
|---|---|
| `~/.hermes/hooks/worktree-ensure.sh` | §4 |
| `~/.hermes/hooks/quiesce-check.sh` | §5 |
| `~/.hermes/hooks/lane-gate.sh` | §6.1, reads `[lane.<name>.gates]` |
| `~/.hermes/hooks/lane-merge.sh` | §6.2 |
| `~/.hermes/hooks/worktree-prune.sh` | §7 |
| `~/.hermes/hooks/lease-{acquire,release}.sh` | `workspace_leases` via the scheduler API |
| `~/.hermes/hooks.toml` | the set (S13 §3.1) |

---

## 9. Edge cases

- **Crashed tick, dirty tree.** Next tick's `worktree-ensure` preserves the dirt and prints
  `dirty_preserved`; the lane's prompt must begin by inspecting `git status`. Never reset.
- **Branch already merged, still checked out.** `worktree-ensure` refreshes it to the base;
  `ahead=0`; `lane-merge` exits `10`; prune keeps the branch.
- **Two lanes of one project tick at once.** Separate branches, separate trees; the lease is
  per (repo, branch, path), so both proceed. Only the base-branch merge is serialised — the
  `git merge --ff-only` in a shared repo takes `index.lock`, and `lane-merge` retries 3× at 2 s
  before exiting `75`.
- **Repo has no `origin`.** `worktree-ensure` exits `65`; the lane fails closed. (A lane that
  cannot fetch must not run on a base it cannot name.)
- **Base branch renamed.** `[lane.<name>].base` is explicit; a rename is an operator edit, not
  a guess.
- **Submodules.** Not handled. A repo with submodules needs `--recurse-submodules` in the
  bootstrap step; until then `worktree-ensure` exits `65` when `.gitmodules` exists.
- **Large tree / slow disk.** `worktree add` of a 1 GB repo takes seconds; the step's
  `timeout_s` is 120 and it is never run in parallel with itself (lease).

---

## 10. Testing

All tests build a throwaway repo (`git init --bare` + a working clone with two commits) in
`t.TempDir()`; none touch a fleet repository.

| # | Test | Expectation |
|---|---|---|
| W1 | ensure on a missing path | tree created on `lane/<p>/<r>`, base sha printed, exit 0 |
| W2 | ensure twice | second run changes nothing, identical stdout line |
| W3 | ensure with dirty tree | `dirty_preserved`, HEAD unchanged, no `reset` in the reflog |
| W4 | ensure with stale directory (not a worktree) | prune + add, exit 0 |
| W5 | ensure when the branch is checked out elsewhere | exit 75, nothing created |
| W6 | ensure with free space below floor | exit 78, no directory created |
| W7 | ensure with `.gitmodules` | exit 65 with the documented message |
| W8 | quiesce with a `sleep 60` holding the tree as cwd | exit 10, pid printed |
| W9 | quiesce idle | exit 0 |
| W10 | gate red | no merge, `merge=blocked`, branch preserved |
| W11 | merge happy path (2 commits ahead) | ff-only merge, ancestry assertion passes, `landed=1` |
| W12 | merge with a conflicting base | exit 65, `merge=conflict`, branch tip unchanged, worktree still present |
| W13 | merge with zero commits | exit 10, `merge=no_commits`, no push |
| W14 | merge push contention (locked ref) | exit 75, local merge intact, retry succeeds |
| W15 | prune after landed | tree gone, branch deleted |
| W16 | prune after conflict | tree kept, branch kept, reason printed |
| W17 | full POST list with a failed quiesce | gate and merge never run (assert no `hook_runs` rows for them) |
| W18 | ephemeral policy with dirty tree | tree reset (documented destructive case) |
| W19 | two lane ticks same project, different roles | both worktrees exist, both merge, base is linear |
| W20 | ensure after a simulated crash (tree present, `.git/index.lock` left behind) | lock stale-detected and removed only when no pid holds it, else exit 75 |

---

## 11. Hilo impact

Depends on: S13 (runner, ordering, receipts), `workspace_leases` (existing DDL),
`internal/lanehook/role.go`, `fleet-sync.py` (`skip_hooks`, `workdir` mirror).
Depended on by: every lane that writes to a repo; the stand-in workdir retirement
(`~/.hermes/stand-in` 6.6 GB, 153,825 files) which becomes removable once the six families
are re-pointed.

---

## 12. Out of scope

- Worker (wave) worktrees — S12 owns them.
- Remote-node checkouts (`bunker exec`) — a separate spec.
- Automatic conflict resolution. A conflict is preserved and alarmed; resolution is a human or
  a foreman decision, never a hook's.
- Worktree garbage collection beyond the lane's own prune (a host-level sweeper is a separate
  row).
