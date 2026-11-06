# Lane hooks — fresh worktree on PRE, merge + prune on POST

S13/S14. A lane that owns a worktree gets a fresh tree at its declared path
**before** the tick spawns, and its work is landed and the tree pruned **after**
the tick ends. The proof is a `hook_runs` receipt naming the tick — not a config
file that looks right.

## The moving parts

| Piece | Where |
|---|---|
| Definitions (source of truth) | `~/.hermes/hooks.toml` (`-hooks-file`, reloaded on mtime/size) |
| Resolver / validator / runner | `internal/lanehook/` |
| Wiring into the tick | `internal/scheduler/lane_hooks.go` (`hookPlan`, `runPreHooks`, `runPostHooks`) |
| Scripts | `scripts/hooks/*.sh` → installed to `~/.hermes/hooks/` |
| Receipts | `hook_runs` (tick_id, phase, level, hook_id, priority, seq, command, **argv**, state, exit_code, duration_ms, output) |
| Operator preview | `schedulerd hooks preview --lane <lane> [--tick <id>] [--json]` |

## Ordering

`hook_order = "level_first"` (the default):

* **PRE** — priority **ascending**, global → namespace → lane. A `git fetch`
  (10) therefore runs before `worktree-ensure` (20).
* **POST** — priority **descending**, lane → namespace → global. `quiesce`(95)
  runs before `merge`(80) before `prune`(20): nothing merges while a process
  holds the tree, nothing prunes before it merged.

## Failure contract

| Phase | `on_failure = closed` | `on_failure = open` |
|---|---|---|
| PRE | the tick does **not** spawn; the row is `TickFailed` with `error = hook_failed:<id>`; **POST still runs** so a lease taken in PRE is released | recorded, the tick spawns |
| POST | the remaining POST steps are skipped; the tick outcome is untouched | recorded, the rest still run |

Exit codes the scripts use: `0` ok · `10` skip / nothing to do (merge
`no_commits`; `worktree-ensure` on a workdir that **is** the repo's primary
checkout) · `65` blocked (conflict, dirty tree, unexpected state — work
preserved) · `75` retryable contention (branch checked out elsewhere, stale
index lock) · `78` precondition unmet (disk floor, not a repo, no
`origin/<base>`, no `pytest`).

A `10` is a named skip, not a failure: the runner records the step as
`state=skipped` in `hook_runs` (with the child's captured output, e.g.
`reason=workdir-is-primary-checkout`), emits a `hook.<phase>.skipped` event,
does not retry it, and never lets it veto the tick — a lane whose workdir
equals the primary checkout ticks without the hook demanding the shared tree
change branch (SCHED-GAP-16991).

## The branch is `lane/<project>/<role>`

`lanehook.RoleFor` splits a lane name into project and role; `lane-lib.sh`
reproduces that rule for the shell scripts, and both scripts take
`--project/--role` so they never re-derive it. `code-factory-qa` is project
`code-factory`, role `qa` → branch `lane/code-factory/qa`.

The earlier spelling turned every dash into a slash, so the same lane produced
`lane/code/factory/qa` — the project name `code-factory` cut in half at its own
dash. Two sibling lanes still got distinct branches, which is why it survived:
the damage was a fabricated path segment that nothing looked for.

## The command a hook sends

A definition's `command` + `args` are templated over:

`{lane} {project} {role} {namespace} {workdir} {repo} {tick} {phase} {hook}`

An unknown placeholder is a **load error** (it used to reach the command as
literal text). To see the exact argv a lane will run, without running it and
without a daemon:

```
$ schedulerd hooks preview --lane code-factory-qa \
    --repo /home/kara/code-factory --workdir /home/kara/proj-wt-code-factory-qa

lane            code-factory-qa
project / role  code-factory / qa
branch          lane/code-factory/qa
executes        no — this is a preview

PRE (2 step(s))
  [1] repo-fetch  level=global priority=10 timeout=60s on_failure=open
      /usr/bin/git -C /home/kara/code-factory fetch --prune origin
  [2] worktree-ensure  level=lane priority=20 timeout=120s on_failure=closed
      /home/kara/.hermes/hooks/worktree-ensure.sh --lane code-factory-qa \
        --project code-factory --role qa --repo /home/kara/code-factory \
        --worktree /home/kara/proj-wt-code-factory-qa --base main --policy persistent --floor-gib 8

POST (3 step(s))
  [1] quiesce-check  level=lane priority=95 timeout=60s on_failure=closed
      /home/kara/.hermes/hooks/quiesce-check.sh --worktree /home/kara/proj-wt-code-factory-qa
  [2] lane-merge ... --push 0
  [3] worktree-prune ... --merged 1
```

The same run with `--json` is what a foreman or a UI should consume; every
receipt also stores the resolved argv, so "what did we actually send?" is
answerable after the fact.

## User stories (executed by `scripts/hooks/test-hooks.sh`)

Each story is a named assertion in the battery; the battery runs against a
throwaway origin + clone, never a fleet repo.

1. **Fresh tree** — *Given* a lane with no worktree, *when* the tick starts,
   *then* `worktree-ensure` creates it at the declared path on
   `lane/<project>/<role>`. (`W1`, `S1`–`S5`)
2. **Idempotent** — *Given* a tree that already exists and is clean, *when*
   ensure runs again, *then* it is byte-identical. (`W2`)
3. **Dirty work survives** — *Given* uncommitted changes, *when* ensure runs,
   *then* nothing is reset and the output says `dirty_preserved`. (`W3`)
4. **Merge lands and is proven** — *Given* a lane branch with commits, *when*
   the POST chain runs, *then* the branch is an ancestor of the base, every
   changed file is present in the landed tree, and the tree is pruned. (`W11`,
   `W15`, `S7`)
5. **Nothing to merge** — *Given* no commits, *when* merge runs, *then* it exits
   `10` with `merge=no_commits` and nothing changes. (`W13`)
6. **Conflict preserves work** — *Given* a conflicting base, *when* merge runs,
   *then* it exits `65`, the worktree is intact and the rebase is aborted.
   (`W12`)
7. **Preconditions refuse loudly** — *Given* a branch checked out elsewhere,
   *then* `75`; *given* disk below the floor, *then* `78` and nothing created;
   *given* a lane name containing `/`, *then* `65`. (`W5`, `W6`, `S6`)
8. **A busy tree is never merged** — *Given* a process with its cwd inside the
   tree, *when* `quiesce-check` runs, *then* it exits `10` naming the pid.
9. **A veto fails the tick but still releases** — *Given* a PRE step that exits
   `65`, *when* the tick runs, *then* the tick is `TickFailed` with
   `hook_failed:<id>` and the POST list still executed.
   (`TestLaneHooks_PreVetoFailsTickAndPostStillRuns`)

## Arming procedure

1. `bash scripts/hooks/install-hooks.sh` — installs the scripts (and copies the
   example to `~/.hermes/hooks.toml` on first run).
2. Edit `~/.hermes/hooks.toml` — one PRE + three POST blocks per lane.
3. `schedulerd hooks preview --lane <lane> --repo … --workdir …` — read the
   argv. Warnings name anything missing (no `[lane]` block, empty `{workdir}`,
   a plan that is empty).
4. Arm **one** lane. Let a tick run.
5. `sqlite3 ~/.hermes/coding-hermes/scheduler.db "select tick_id,phase,hook_id,state,exit_code,argv from hook_runs order by run_id desc limit 10"` —
   receipts, or it did not happen.
6. Only then widen. Which lanes may land their own commits (`lane-merge` with
   `--push 1`) is an owner decision, not a default.

## Known limits

* A lane's `{workdir}` comes from the project row; `hooks.toml` declares the
  policy for it. If they disagree the row wins, so a preview must be given the
  same `--workdir` to be exact.
* `gates` in `[lane.*]` are declared but not executed by the hook chain yet —
  the lane's own tick runs them.
* The scripts are POSIX `sh`. `lane-lib.sh` must be installed next to them
  (it is; `install-hooks.sh` copies every `*.sh`).
