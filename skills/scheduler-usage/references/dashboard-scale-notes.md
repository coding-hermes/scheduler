# Dashboard + API at fleet scale — verification recipe (2026-09-25, 496 lanes)

How to verify the scheduler's operator surfaces WITHOUT guessing: exact commands,
current numbers, and the traps that produce false verdicts. Read with
docs/dogfood/2026-09-25-scale-dashboard.md (the run these came from).

## 1. Timing battery (the numbers a fresh run should beat)

```bash
# warm server-side render times, n=10, client overhead ~2ms
hyperfine --warmup 2 --runs 10 -N \
  'curl -s -m 60 -o /dev/null http://127.0.0.1:9090/' \
  'curl -s -m 60 -o /dev/null http://127.0.0.1:9090/dashboard/partial' \
  'curl -s -m 30 -o /dev/null http://127.0.0.1:9090/projects/<lane>'
```

2026-09-25 @ 496 lanes, build 63dfbd10: `/` 3.11s±0.45, partial 3.73s±0.73,
per-project 0.23s. The 10s htmx autorefresh makes the partial the one number that
compounds per open tab. Criterion from DASH-PERF-003 was <3s (verified 0.87s at
closure in-tree) — live is now past it; judge against SCHED-GAP-1623's acceptance.

Fast paths (safe for automation): `/api/v1/events` ~2ms, `/api/v1/queue` ~2ms,
`/api/v1/projects` 0.13-0.16s but 1.15MB (no pagination; `?limit` silently ignored
— SCHED-GAP-1624), per-project pages 0.2-0.3s, `/ticks` 0.2s, `/queue` 0.02s.

Slow-state trap: 1622/PERF-002 make `/api/v1/status` and `/api/v1/health` 504 at
their exact budget deadlines (1s/5s) during evaluate() passes, and answer in
milliseconds minutes later. A single 200 does NOT disprove the row and a single 504
does not prove it urgent — sample across several evaluate boundaries before judging.

## 2. Real-browser cold load (what an operator feels)

```bash
time (timeout 180 google-chrome --headless=new --disable-gpu --no-sandbox \
  --virtual-time-budget=60000 --dump-dom 'http://127.0.0.1:9090/' > dom.html)
grep -c '<tr' dom.html   # 142 = 50 fleet rows + ticks table rendered
```

2026-09-25: ~80s wall to settled DOM (Chrome CPU 3.5s — the wait is server-side).
Warm open is ~4s. Chrome itself is never the bottleneck here.

## 3. Ground-truth cross-check (do not trust one source)

```bash
python3 scratch/dg2_xcheck.py   # fetches / and /api/v1/projects, compares row sets
```

Method: parse the served HTML and the API JSON independently; compare name sets,
weights, counts. 2026-09-25 result: 50 rows/page (pagination), footer "of 496
lanes" == API 496 (332 enabled/164 disabled), weights 12/12 on a random sample,
honest "0 lanes" empty state for bogus searches, page 2 continues the API's
alphabetical order exactly. The dashboard currently does not lie about numbers.

## 4. Profiling the render path (waiting ≠ CPU)

```bash
# CPU profile UNDER load: ReadBoardFreshness ~31% (mostly the wake watcher/packer,
# not the dashboard itself); render CPU itself is low — the handler WAITS.
curl -s 'http://127.0.0.1:9090/debug/pprof/profile?seconds=12' -o cpu.pb.gz
go tool pprof -top -cum -nodecount=40 cpu.pb.gz | grep -E 'dashboard|scheduler\.'
# The wait shape: dump goroutines DURING renders
curl -s 'http://127.0.0.1:9090/debug/pprof/goroutine?debug=2' > g.txt
grep -B2 -A8 'dashboard' g.txt
# expected: handler at generator_data.go:1027 [chan send] + workers in
# os/exec.(*Cmd).Output inside tickWork (generator_data.go:1746) — a git
# subprocess PER TICK SAMPLE per project.
```

Lesson: wall >> CPU means wait on something (subprocesses, locks, DB) — goroutine
dump, not CPU profile, names it. A CPU profile alone blames the wrong component.

## 5. Deployment drift check (board says fixed, box says no)

```bash
curl -s http://127.0.0.1:9090/api/v1/health | jq .build_sha,.build_time
git merge-base --is-ancestor <fix-commit> <build_sha> && echo deployed || echo DRIFT
# Probe a security fix harmlessly: nonexistent-project mutation must NOT reach the DB
curl -s -X POST http://127.0.0.1:9090/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"project_bump",
       "arguments":{"name":"definitely-not-a-project-<suffix>"}}}'
# fixed build: 401/credential error. drifted build: -32000 "project not found" (pre-auth).
```

2026-09-25: fix b16e61a8 closed SCHED-GAP-1619 at 12:01Z; running daemon 63dfbd10
still answered pre-auth at 13:30Z (SCHED-GAP-1626). Always run this before
believing any "fixed" row about a served surface.

## 6. Board state sanity before/after dogfood writes

```bash
tail -c1 .coding-hermes/board/tasks.jsonl | xxd | head -1   # must end 0a (newline)
jq -r '.id' <board> | sort | uniq -d                        # duplicate id census
git diff --numstat -- .coding-hermes/board/tasks.jsonl      # count must match your rows
git show --name-only --format= HEAD                          # commit contains ONLY the board
```

If a sibling lane appends/amends between your read and your commit: commit
HEAD + only-your-rows, then restore the sibling's uncommitted line byte-exactly
(see scratch/dg2_commit_rows.py for the working pattern; numstat afterwards must
show exactly the sibling's 1/1).
