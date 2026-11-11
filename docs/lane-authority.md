# Lane Authority — what a lane may decide alone

> **OWNER RATIFIED 2026-11-11** — Bane, verbatim: "yes i agree with that rule being added"
> (reply to the decision card carrying this exact draft). Standing fleet policy from
> this date; lanes cite this file, not their own judgement, when they decide alone.

Standing classification for every fleet lane (foreman, worker, sync, QA, PM).
Written per SCHED-GAP-16937's acceptance criteria; derived from the
`board-dispatchability-repair` and `owner-decision-briefing` skills and the
owner's standing doctrines. Every lane loads this before composing a tick.

## Why this exists

On 2026-11-07 the scheduler board held 626 rows parked on owner decisions —
"nothing written states what a lane may decide alone", so every unsure lane
escalated. The count had grown ~3.4x by 2026-11-11 (2,156 owner-flavored open
rows across 490 live boards). Escalation is not a safety behavior when the
owner has already ruled on the class: it is a queue that starves the fleet and
drowns the owner. A lane that escalates something this file authorizes it to
decide is doing negative work.

## A lane MAY decide alone — no owner input needed

1. **Code work in its own repos:** fixes, refactors, tests, docs — "why let it
   sit broken" is standing policy; fix and verify, don't file a row to ask.
2. **Re-status its own board rows:** `blocked -> pending` when the blocker
   resolves; re-file a defect into the repo that OWNS the code (name the path
   and the function in the acceptance criteria), then re-status this row.
3. **Housekeeping:** purge dead/duplicate/stale rows and artifacts with
   read-back proof at the other end. This is not an owner decision.
4. **Scope-narrowing within stated intent:** authorizing the free subset
   (an open-source toolchain, no licence server) is a valid unblock — narrow
   the row's acceptance to the authorized scope instead of dispatching the
   parked half.
5. **Rollback/revert of its own automation** and its own uncommitted work.
6. **Filing new rows** for defects it found, with runnable acceptance criteria.
7. **Implementation choices** inside its lane: approach, model selection per
   the routing registry, worktree vs in-place, test strategy.
8. **Closing rows on real evidence:** tier1/guard/judge verdicts read from
   files, never from memory or a worker's self-report.

## A lane MUST stop and ask the owner

1. **Money:** purchases, licences, paid accounts, quota or plan changes.
2. **Credentials:** creating, rotating or storing secrets; anything beyond
   USING credentials that already exist in their sanctioned stores.
3. **Public surfaces:** publishing a repo, a release, a post, a domain, or any
   marketing-facing artifact.
4. **Data that is not the lane's own:** deleting or rewriting user data,
   DuckBrain namespaces, backups, or anything on shared systems.
5. **Security posture:** firewall, auth, exposure, ports, trust stores.
6. **Fleet structure:** enabling/disabling/pausing lanes, changing cadences or
   authority for OTHER projects, scheduler config for projects not its own.
7. **Irreversible operations:** git history rewrites, unregistering domains,
   destructive deletes without a backup.

## Behavior when unsure

- Default to the most recent explicit owner instruction on record, then to
  this file.
- **An unanswered offer is NOT consent.** If the owner has not answered a
  proposal, the proposal is not authorized — this is standing doctrine since
  2026-08-31.
- "Maybe we should X" from the owner means EVALUATE, not authorize.
- If genuinely unclassified: file ONE decision card in the
  `owner-decision-briefing` shape — plain language, the options, a
  recommendation, the cost of delay — never a vague `OWNER:` placeholder,
  never silent parking, never one card per row for the same decision.
- PROPOSED-DEFAULT items close on silence; milestone-gating questions do not.
  Say which kind a card is.

## Disposition of the parked rows

Every owner-flavored row is triaged against this file into exactly one of:

- **lane-authority** — this file authorizes it: unblock, do, close on evidence.
- **owner-gated** — genuinely on the "MUST ask" list: stays blocked with a
  SPECIFIC decision card (never a placeholder).
- **blocked-elsewhere** — the fix lives in another repo: re-filed there,
  this row re-statuses pending or closes as moved.
- **chain** — blocked only by naming another row: re-verify the parent at
  HEAD before deferring; unblock the pair in one action when the parent is
  itself mis-classified.

The triage wave reports the count returned to lane authority. A new row filed
after this file exists follows the rule in its first pass.
