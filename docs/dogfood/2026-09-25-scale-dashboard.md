# Dogfood integration report — coding-hermes-scheduler, 2026-09-25 (496-lane scale sweep)

Tick: coding-hermes-scheduler-dogfood-2026-09-25-12-53-47 · daemon under test: v1.5.0-92-g63dfbd10 (systemd user unit, live fleet, 496 registered lanes / 332 enabled)

## The angle

This morning's tick (05:49) dogfooded the mutating control surface (REST + MCP) and
filed SCHED-GAP-1619..1622. The 09-23/09-24 runs covered deploy groups and the
dashboard/status perf surface — but at 332-485 projects, before the fleet crossed
500 registered lanes. This run took the one surface that changed most since: **the
dashboard as a human operator's information surface at the new scale**, plus the
fresh-install leg the morning run launched but never collected.

Promise under test (README): "You can monitor, pause, or adjust any project through
the dashboard, REST API, or MCP tools," with the fleet overview and health panel
refreshing every 10 seconds.

## What I did (real operator flow)

Opened the dashboard the way an operator would (headless Chrome, cold), then
cross-examined every headline number against independently computed ground truth
(`GET /api/v1/projects` parsed separately from the served HTML), used the shipped
search/filter/pagination controls for real, timed the pages an operator waits on
(hyperfine n=10), profiled the daemon under that load (pprof + mid-render goroutine
dump), and ran a fresh install from the public repo on an ephemeral bunker agent.

## Numbers (all measured 2026-09-25, warm unless stated)

| Surface | 09-24 (332-485 lanes) | Now (496 lanes) |
|---|---|---|
| `GET /` server-side | 45.9s cold, 2.2s warm | 3.11s ± 0.45 warm (n=10); cold-to-settled DOM ~80s in Chrome |
| `/dashboard/partial` (10s autorefresh) | 4.05s ± 7.78 | **3.73s ± 0.73 (n=10)** |
| `/api/v1/status` | 94.7s | 0.26-0.29s |
| `/api/v1/health` | 26.4s | 0.5-0.6ms |
| `/api/v1/projects` | 0.14s | 0.13-0.16s, 1.15 MB for all 496 |
| `/api/v1/queue` | 4.5ms | 1.6-2.1ms |
| `/queue` `/ticks` `/health` pages | fast | 0.02s / 0.20s / 0.23s |
| per-project page `/projects/<name>` | 0.17s | 0.20-0.31s (200 OK, honest 404 for unknown) |
| search render `/?q=…` | n/a (no search) | 5.7-7.0s |

The fleet is dramatically faster than 24h ago (the SCHED-GAP-1575 mutex/convoy fixes
landed and deployed). SCHED-GAP-1622's 504 symptoms are now INTERMITTENT rather than
constant: the supervisor's re-measure (13:01-13:06Z, now in the row's foreman_note)
caught `/api/v1/health` 504 x10/10 and `/api/v1/status` 504 x3/3 at exact budget
deadlines; my probes minutes later (13:10-13:20Z) got 200s (health 0.5ms, status
0.26s). That windowing is consistent with the evaluate() write-lock cadence the
supervisor narrowed to (1622 = second face of PERF-002), not with a constant
per-request cost.

## What works for a real operator

- The SCHED-GAP-1598 controls are deployed and functional: search ("search lanes…",
  `?q=`), filter selects (project/outcome/size/sort/dir per table), page-size, and
  pagination. Search for `coding-hermes-scheduler` returns the 8 family lanes;
  a bogus query renders an honest "0 lanes" empty state.
- Numbers agree with ground truth: fleet table row weights matched
  `GET /api/v1/projects` 12/12 on a random sample; page footer says "of 496 lanes"
  and the API says 496 (332 enabled / 164 disabled — both visible on the page);
  page 2 starts exactly where the API's alphabetical list continues.
- Disabled lanes render with their parent relationship (`└ X-pm ↳ X (L1)`).
- Per-project pages, queue, ticks, health panel: all sub-second, honest 404s.

## What broke / regressed

### F1 (P1): the 10s autorefresh partial regressed past its own closed criterion

DASH-PERF-003 closed 2026-08-17 with "warm /dashboard/partial 0.87s x3 (<3s
criterion)". Today at 496 lanes: **3.73s ± 0.73 (n=10, range 2.48-5.10s)**, and `GET /`
3.11s ± 0.45. The refresh budget is 10s; an operator with the dashboard open waits
~3.7s of fresh data latency every cycle, and the Chrome cold path (~80s to settled
DOM) makes the first open of the day painful.

Mechanism (measured, not guessed):
- pprof under render load (12s, 48.75% CPU): `ReadBoardFreshness` 31.3% cum
  (classifyCompleteRow 26.5% inside it), called 53.5% by the board-wake watcher and
  46.5% by the packer's PendingTaskCounter — the PERF-002 signature, independently
  reconfirmed at 496 lanes (that row stays open and now has a second measurement).
- Mid-render goroutine dump (live, during 10 back-to-back renders): the render
  handler blocked in `dashboard.(*Generator).enrichProjects` on the bounded pool
  channel (generator_data.go:1027) while pool workers sat in
  `os/exec.(*Cmd).Output` inside `tickWork` (generator_data.go:1746) — the render
  runs a `git log --since/--until --pretty=%s` SUBPROCESS per tick sample per
  project. At 496 lanes with 8-way concurrency that is hundreds of fork/execs per
  render; the wall time (3.7s) vs low render CPU is the gap.
- `enrichProjects.func1` also calls `readBoardSteps` per project per render.

Fix direction (owner's call): snapshot freshness + enrichment (compute in the
background on a cadence, serve the snapshot to renders), cache/batch the tickWork
git reads (or replace with gitreins history reads already collected), and keep
renders off the Loop critical section. Do NOT reopen DASH-PERF-003 (it was true at
its merge); file the regression against the deploy/verify step (see F3) plus this row.

### F2 (P1): the running daemon predates today's SCHED-GAP-1619 auth fix — live drift re-proven

SCHED-GAP-1619 (unauthenticated MCP mutations execute on the live fleet) was fixed
in commit b16e61a8 (2026-09-25 11:44Z, +222 lines of internal/mcp/auth.go) and the
row closed at 12:01Z. The RUNNING daemon is build 63dfbd10 built 2026-09-25T01:28:07Z —
b16e61a8 is NOT an ancestor of it (merge-base check). Live probe on the running build:

```
curl -s -X POST http://127.0.0.1:9090/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"project_bump",
       "arguments":{"name":"definitely-not-a-project-dg2"}}}'
→ {"error":{"code":-32000,"message":"project not found: definitely-not-a-project-dg2"}}
```

No 401, no operator credential demanded — the request reached the database handler
(pre-auth), exactly the morning run's finding, still live at 13:30Z. The probe was
deliberately against a nonexistent project so it could not mutate anything; tools/list
also answers 200 unauthenticated. "Fixed" (board) and "deployed" (process) are
different facts; the second one is the gap. Same drift explanation covers DASH-PERF-003:
its 0.87s verification ran in the worker tree, the running process serves older code.

### F3 (P2): `GET /api/v1/projects` ships 1.15 MB and ignores `limit` silently

496 projects, no pagination (docs say "Query params: none" — so by design), but
`?limit=5` returns all 496 with HTTP 200 while sibling list endpoints (`/api/v1/events?limit=5`,
`/api/v1/ticks?limit=3`) honor `limit`. A param that is silently ignored trains
integrations to ship unbounded responses. Every dashboard poller and stand-in script
(e.g. standin-pick.py) pulls the full 1.15 MB per call. Fix: document the absence
explicitly, or reject unknown params with 400, or add real pagination.

### F4 (P2): mutating dashboard controls still do not exist

6+ weeks after the 09-24 run flagged "no pause/resume/adjust controls in the UI",
the dashboard's only buttons are sidebar toggles and filter Apply submits. The README
sentence "You can monitor, pause, or adjust any project through the dashboard…" is
still one-third false. No board row owned this specific gap (SCHED-GAP-1598 owns
read-side table controls and is complete). Filed.

### F5 (P2): MCP mutations still write no audit event on the running build

Companion to F2: until b16e61a8 deploys, MCP mutating calls execute with neither auth
nor an audit row (the morning run's secondary defect). When the deploy happens, the
verify step must include an audit-trail assertion, not just a 401.

### F6 (P2): the morning tick's ephemeral bunker battery was never collected

The 05:49 tick's report says "Bunker leg: LAUNCHED (bunker-las-03 agent c376f19f,
full battery) — collect in flight at report time." That agent no longer exists on
bunker-las-03 (list shows only df-renew-0925 and 6963ad23 from that window), and no
trace of the agent id or its results exists in the stand-in workdir, /tmp, the repo,
or the board. An install leg that outlives its tick with no checkpoint loses its
evidence permanently. Process fix: the install leg must write its spawn-id + result
path to the scratch dir BEFORE the tick's report, and the follow-up must collect
within the TTL.

## The fresh-install leg (this run) — PASSED

bunker-las-03 agent 6bd25844 (bare Debian user, no toolchains), destroy at the end:
- Phase A (README path verbatim): `git clone https://github.com/coding-hermes/scheduler.git`
  → OK (45 MB); `make build` → Error 127 (Go missing — README prerequisite #1, correctly
  documented as a prerequisite but not auto-installed; sqlite3 prerequisite #3 also
  absent on bare Debian). Phase A time 34s.
- Phase B (minimal user bootstrap): downloaded go1.26.6.linux-amd64 from go.dev
  (first attempt died curl 23 mid-write — retried), untar to ~/toolchain, `make build`
  → **63s**, `./bin/schedulerd --version` → exit 0.
- Smoke: booted with a throwaway DB (`--db /tmp/dg-smoke.db --config /dev/null`),
  `GET /` → 200, 21.7 KB in **6.0ms**; `GET /api/v1/health` → 200 in **0.5ms**.
  A fresh user on a clean machine goes clone→running in ~2 minutes with one
  toolchain download.

## Verdict

🟡 **PROMISING-BUT-ROUGH** (monitoring surface at 496 lanes). The information a
dashboard should give an operator is now correct, searchable, paginated, and
cross-checkable against the API — a big step up from 24h ago, and the fresh-install
story is genuinely good (2 minutes, honest prereqs). What keeps it rough: the 10s
autorefresh partial regressed past its closed criterion (3.73s), the fix for the
worst security finding of the week is merged but not deployed (live pre-auth MCP
re-proven at 13:30Z), and "closed with verified numbers" rows have no deploy-time
re-verification gate. Trust the board's numbers; verify the process before trusting
the box.

Time-to-first-success (dashboard open, this run): ~80s cold Chrome; ~4s warm.
Friction count: 3 (3.7s refresh partial, 1.15MB list responses, missing mutating controls).

Left behind:
- `docs/dogfood/2026-09-25-scale-dashboard.md` (this file)
- `docs/dogfood/diagnostics.md` — appended lessons 6-8
- `skills/scheduler-usage/references/dashboard-scale-notes.md` — scale + drift verification recipe
- Board rows SCHED-GAP-1623..1628 + INSTALL row (see tick report)
- Evidence: `~/.hermes/stand-in/dogfood/coding-hermes-scheduler/scratch/` (dg2_* files: hyperfine TSV, pprof, goroutine dump, cross-check script + outputs, install logs)
