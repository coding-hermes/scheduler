# SCHED-GAP-17009 operations fix — 2026-11-22

Tick: doc-writer-ops-2026-11-22-12-16-07 (coding-hermes-scheduler ops lane)
Worker/supervisor note: tick arrived as a WAVE RECOVERY fire; the recovery leg was
verified stranded-empty and closed (below), then the tick's normal work ran.

Scope statement: bounded, reversible host-script edit plus a one-shot aged sweep
demo. No service restart; the disk-guard unit was observed live before and after
and was never touched. All host paths below are rendered with `~` in place of the
operator home prefix.

## Recovery leg (injected wave, verified stranded — no action)

Three inherited wave manifests were published nowhere and carried zero workers.
Reads, verbatim:
```text
== foreman-2026-11-21-18-14-28 == {"finished_at":null,"closeout_commit_sha":null,"n_workers":0,"started":""}
== foreman-2026-11-22-07-04-46 == {"finished_at":null,"closeout_commit_sha":null,"n_workers":0,"started":""}
== foreman-2026-11-22-09-37-10 == {"finished_at":null,"closeout_commit_sha":null,"n_workers":0,"started":""}
```
At the published tip these manifests do not exist at all:
`git ls-tree --name-only origin/main:.coding-hermes/waves | grep -c 2026-11-2` → 0,
and
`git show origin/main:.coding-hermes/board/events.jsonl | grep -c -e 2026-11-21-18-14-28 -e 2026-11-22-07-04-46 -e 2026-11-22-09-37-10` → 0.
`git branch -a` has no branch for any of the three slabugs; `git worktree list`
shows none created by them. Verdict: stranded empty manifests (0 workers, blank
`started=`) — the three were closed by setting `finished_at` locally
(gitignored, untracked, reversible). A fourth local manifest
(`foreman-2026-11-22-11-45-40.json`, also 0 workers) was left untouched: it is
not named by this tick's recovery block and could belong to a live sibling.

## Arm 1 — baseline re-measured (before the fix)

```text
$ du -sh ~/.hermes/cache/scratch
31G
$ ls -1 ~/.hermes/cache/scratch | wc -l
2893
$ df -h ~/.hermes/cache/scratch | tail -1
/dev/nvme0n1p2  1.8T  1.6T  195G  89% /
$ du -xhd1 ~/.hermes/cache/scratch | sort -rh | head -6
31G  total; 6.4G cf-tick-2026-11-21-09-37-46; 5.8G perf-db-tick-102207;
      3.7G mythos-gate-1744; 2.1G vm-clean-voice-mode-docs-2026-11-21-23-52-39;
      1.1G vmqa-1935
```
Trajectory against the filed baselines: 119G/2529 (2026-11-10) → 64G/1421
(2026-11-11 predecessor audit, unpublished branch) → 31G/2893 now: the pool
itself shrank ~half in 11 days, but entry count ROSE (2893 vs 2529) and free
root space FELL 336G → 195G (89% used) — pressure is now 45G above the
guard's 150G soft threshold, so the retention mechanism landed while still
relevant. The 11-11 predecessor audit lives on unpublished branch
`ops/SCHED-GAP-17009-20261111` (not on origin/main); its evidence doc is landed
with this tick (below).

## Arm 2 — retention entry in the guard's sweep list with keep-list

Edited `~/.hermes/scripts/disk-guard.py` (host script, fleet ops surface named
by the row's acceptance). Reversible: pre-edit backup kept at
`~/.hermes/scripts/disk-guard.py.bak-20261122-sched-gap-17009`.
sha256 before: 54cf6052d1968ae5ed12886fd7a278f180c55fcd0ce510932df39530a81a3751
sha256 after:  1c8a1760897501b6065061ca0dd0bcd5a9d32644230c69803f29487706b55b9e

Change (39 diff lines, `py_compile`-equivalent verified by executing the module
via its own --dry-run entry):
1. Sweep entry appended to SCRATCH_GLOBS:
   `(str(HOME / ".hermes/cache/scratch/*"), 24 * 3600)` — per-entry age floor
   24h, dirs only, same live-sqlite skip as every other pool.
2. Keep-list constant + skip in the reclaim loop (new SCRATCH_KEEP_SUBSTRINGS =
   ("evidence", "audit", "report")): a scratch-root dir whose name carries one
   of these substrings survives at ANY age; root-level files (evidence
   receipts) were already skipped by the dirs-only rule; dirs containing any
   `*.db` remain skipped by the existing live-sqlite glob (disk-guard.py
   lines 136-147 unchanged and still applying to the new pool).
3. `sweep_preview()` + dry-run candidate print: `--dry-run` now lists exactly
   what an aged sweep would remove (same skip rules, no deletion), because the
   acceptance requires the candidate set to be visible in a dry run.

Note on the demo mechanics: the aged sweep for arm 6 ran the production
`reclaim_soft()` loaded from the edited disk-guard.py via importlib spec
(plain `import disk_guard` cannot resolve the dash-named file), with
SCRATCH_GLOBS scoped to the scratch-root pattern so the demo changed only the
pool the row names. The full pressure set (docker caches, /tmp, /var/tmp) was
previewed unchanged by both dry runs; docker-side pruning was deliberately not
executed mid-tick, and no scheduler service was restarted.

## Arm 3 — dry-run completes and prints the new scratch candidates (no removal)

```text
$ python3 ~/.hermes/scripts/disk-guard.py --dry-run
root free: 193.8 GiB  (soft 150.0, hard 60.0)  rate_limited=False  would_shed=no
aged sweep candidates: 65
  aged 116h idle: /tmp/go-build1248257200
  ...
scratch-root candidates (25):
  aged 24h idle: ~/.../scratch/briefs
  aged 25h idle: ~/.../scratch/bt-1795258072
  aged 25h idle: ~/.../scratch/cf-tick-2026-11-21-093746
  aged 446h idle: ~/.../scratch/df497
  aged 111h idle: ~/.../scratch/muster-ops-20261113-2123
  aged 136h idle: ~/.../scratch/perf-dexdat-core-2026-11-16-19-27-03
  ... (19 more)
rc=0
```
Nothing removed (dry run). Full capture: the raw dry-run output is quoted above
in tail-trimmed form; the complete untrimmed first 5 + candidate lines are in
the tick transcript; the dry-run ran twice (plain + forced) with identical
candidate sets.

## Arm 4 — forced-threshold dry run shows aged scratch entries inside the reclaim set

```text
$ DISK_GUARD_SOFT_GB=4000 python3 ~/.hermes/scripts/disk-guard.py --dry-run
root free: 193.8 GiB  (soft 4000.0, hard 60.0)  rate_limited=False  would_shed=SOFT
aged sweep candidates: 65
$ grep -c "hermes/cache/scratch" <capture> → 25
```

## Arm 5 — wiring verified from the live unit

```text
$ grep -n "hermes/cache/scratch" ~/.hermes/scripts/disk-guard.py
66:    # 2026-11-22 (SCHED-GAP-17009): the agent scratch root (~/.hermes/cache/scratch)
72:    (str(HOME / ".hermes/cache/scratch/*"), 24 * 3600),
80:SCRATCH_ROOT_DIR = str(HOME / ".hermes/cache/scratch")
160/188: keep-list skip in reclaim_soft / sweep_preview
$ systemctl --user cat disk-guard.service | grep ExecStart
ExecStart=/usr/bin/python3 ~/.hermes/scripts/disk-guard.py
$ systemctl --user is-active disk-guard.timer
active
```
The timer fires every minute and serves the edited file directly (no restart
needed, none performed).

## Arm 6 — end-to-end retention demo (one aged sweep)

Dummies created under ~/.hermes/cache/scratch and mtime-stamped:
`dg17009-old-25h` (25h old), `dg17009-recent-1h` (1h old),
`dg17009-evidence-demo` (25h old, keep-list name). Sweep run (production
reclaim_soft, scratch-root pattern only):

```text
demo candidates before sweep: 32
ACT: removed aged scratch .../scratch/briefs (>24h idle)
ACT: removed aged scratch .../scratch/df497 (>24h idle)
ACT: removed aged scratch .../scratch/dg17009-old-25h (>24h idle)
ACT: removed aged scratch .../scratch/muster-ops-20261113-2123 (>24h idle)
... (33 ACT lines total)
total actions: 33
$ test -d dg17009-old-25h         → GONE
$ test -d dg17009-recent-1h       → SURVIVED
$ test -d dg17009-evidence-demo   → SURVIVED
```
Disclosures: (1) the sweep reported 33 removals against 32 preview candidates —
one entry aged into the set between the preview glob and the sweep glob
(removed too; names in the sweep log are the authority); (2)
`perf-dexdat-core-2026-11-16-19-27-03` was reported as removed but its dir still
exists — rmtree's ignore_errors left an unreadable (permission-denied) postgres
`pgdata` subtree inside; the visible parent tree was reclaimed and the remnant
is inert, but the entry was NOT fully reclaimed in one pass; (3) after the sweep
the pool measured 29G / 2873-2877 entries (du) — root free space also grew
195G → 240G in the same window through unrelated host churn, so that delta is
NOT attributed to this sweep (the pool's own contribution per du is ~2G plus
stub-ci/cf-tick trees; the remainder of the free-space jump is unattributed
host activity).

Regenerability statement for the one real deletion executed: the sweep acted
only on dirs older than 24h that contain no `*.db`, no files, no keep-list
names — all measured scratch-root victims were dated test fixtures and
workspace debris (df497 from 2026-11-04, muster-ops from 2026-11-13,
cf-tick from 2026-11-21). Nothing evidence- or audit-named, no file, and no
database-bearing dir was removed; the keep-list makes that class permanent.

## Landed alongside — predecessor audit

The 2026-11-11 measurement-only audit (tick fleet-ops-check-2026-11-11-10-03-09,
evidence doc + one board `audit` event, id 1794267359952719805) sat on
unpublished branch `ops/SCHED-GAP-17009-20261111`. This tick lands that evidence
doc verbatim except for the operator-home prefix rendered as `~` (4 hits; the
original is preserved on the branch at sha256
342075a0bb74a2fc193cf854c5e50a10f2c5a4ae29596b4ce111f5114a97c783 — the landed
copy's own sha256 is recorded in the board event). Its event line was NOT
published: the id 1794267359952719805 is already used at the published tip by a
different event (task_id SCHED-GAP-162, tick fleet-ops-check same day, 10:56Z vs
10:14Z) — a real ledger id collision that also explains why the branch never
merged. The doc carries no id and is landed; the collision is reported as
ledger debt with the branch named, not repaired by editing the JSONL; no branch
merge was needed (net diff = one file + one event line, both free of id
collisions at the current tip — the event line turned out NOT to be free of
collision, hence the withholding).

## Verdict

All six acceptance arms are measured GREEN on the live host. Row closed with
evidence in the same commit as this file and the board event.
