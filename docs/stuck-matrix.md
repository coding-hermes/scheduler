# Stuck-state matrix — who resolves what

**Owner directive (2026-11-07):** when a board is stuck, the lane must know which agent unsticks it. Blockers that are reversible — a release cut, a dirty branch, a stale lock — are resolved by the fleet. Waiting for a human is the worst possible option; we are building an automated code factory, not a queue of questions.

Machine-readable source: [`stuck-matrix.json`](stuck-matrix.json). This page is the human view of the same data.

## The rule

> **Reversible → resolve now, with the command below. Irreversible → escalate once, with the measured condition and the default action you will take if no answer arrives.**

A tick that ends "blocked, waiting for the owner" on a reversible class is a **failed tick**, not a conservative one. A repeat escalation on an irreversible class means the escalation path is broken, not that the owner forgot.

## Matrix

| # | Symptom | Reversible | Resolver | Command that resolves it | Escalate only when |
|---|---|---|---|---|---|
| 1 | `could not write index` / `stash failed` / unexplained lock | ✅ | this lane | `fuser -v .git/index.lock` → `ps -o stat,etime -p <pid>` → if D-state **reader** for minutes: `rm .git/index.lock` | the holder is a **mutating** command (commit/merge/rebase) |
| 2 | Worktree dirty with paths the tick did not write | ✅ | foreman (attribute) | `git status --porcelain` + `git diff --cached --text`; harness state → `git checkout HEAD -- .gitreins/tasks.yaml` | the paths are a **live sibling's** real edits |
| 3 | Branch diverged / commits unpushed | ✅ | foreman | `git fetch origin && git rebase origin/main && git push origin main`, then `git ls-remote` == `rev-parse HEAD` | never for a landable divergence |
| 4 | Guard FAIL whose finding was already true at base | ✅ | foreman | prove it with `git stash --include-untracked && <gate> && git stash pop`, then commit with that proof quoted | the gate fails identically on a clean `origin/main` — then the **gate** gets the row |
| 5 | gitreins judge FAIL / INCOMPLETE | ✅ | foreman | read `.gitreins/history/<date>/<id>/verdict.json`, fix the cited criterion, rerun **sequentially** | the cited criterion needs an owner decision — the **row** escalates, not the judge |
| 6 | Merge conflict | ✅ | foreman / merge-reconciler | `git merge-tree --write-tree origin/main HEAD`; resolve; re-run gates on the merged tree | both sides changed one semantic contract and no test expresses intent |
| 7 | Lane never spawns / ticks starve | ✅ | scheduler operator | `curl -s localhost:9090/api/v1/projects/<lane>`; fix the pin (toml **and** DB), force one tick, verify the tick row | the fix needs spend or raises concurrency past the load gate |
| 8 | Model/provider refusal (402, 429, circuit open, wrong seat) | ✅ | foreman | `router circuit clear --all`; dispatch; verify the **served** model in `session_model_usage` | the account is exhausted or a new account is needed |
| 9 | Dead model pin | ✅ | foreman | fix the pin in `fleet.toml` **and** the DB row; re-dispatch; verify the served model | the replacement changes the cost tier the owner chose |
| 10 | Blocked on a row that is already complete | ✅ | foreman / PM | `boardctl show <id> --json`; prerequisite live → `--status pending` and drop the stale reason; still dead → **replace** the blocker | never |
| 11 | Environment down (service/container/host) | ✅ | foreman | `docker inspect` / `systemctl --user show`; a clean exit under `restart=unless-stopped` is **stoppable** — restore it and re-run the row's arms | the component is retired (renamed/migrated) — then re-scope |
| 12 | Duplicate rows / reused ids | ✅ | PM | `dedupe-board --dry-run`; keep the row with evidence, close the twin citing the survivor | never |
| 13 | Release cut is ready | ✅ | release-engineer | cut the tag on the verified commit; a tag is reversible until consumers depend on it | the release carries a breaking change or a version-policy decision |
| 14 | **Deletion of live data, secrets, spend, policy, doctrine conflict** | ❌ | **OWNER** | — | always: one ask, exact question, options, and your default |

## Why this exists

Before this matrix the fleet's most common resolution was *silence*: a lane hit a dirty branch or a stale lock, called it a barrier, wrote a receipt, and stopped. Four open rows were waiting on the owner for things a script could have done (`SCHED-GAP-162`, `REL-GUARD-001`, `SCHED-GAP-16934`, `QA-AUGER-13`). The barrier discipline is correct — do not trash a sibling's work — but its **outcome** must be *attribution and resolution*, not *waiting*.

## Using it in a tick

1. Name the symptom in the tick's own words.
2. Find the row above. If there is no row, that is a **gap**: file it against the matrix, and resolve the incident by hand this once.
3. Run the command. Record the output as the tick's evidence.
4. Only if the "escalate when" condition is *measured* — not suspected — escalate, once, with the default action stated.
5. If the same escalation arrives twice, the escalation path is the defect.
