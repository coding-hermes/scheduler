# Dogfood 2026-10-20 — the cadence read surface, and what the control plane costs

**Tick:** `coding-hermes-scheduler-dogfood-2026-10-20-06-54-47`
**Target:** repo HEAD `e209c70b` (`main`), live daemon `v1.12.0-488-gd6e17be8` on `127.0.0.1:9090`,
608 projects / 411 enabled, host load ~23 (16 cores).
**Lane contract:** read-only — no source, scheduler state, fleet config or repo visibility was modified.
Every write in this tick is this report, the run-history entry in `.coding-hermes/dogfood-log.md`,
and four board rows.
**Rows filed:** `PERF-002-R2`, `SCHED-GAP-1745`, `SCHED-GAP-1746`, `DOGFOOD-030`.

## 1. The promise being tested

`docs/api.md` §10 (`GET /api/v1/cadence`, documented by DOC-19 hours before this tick):

> "configured vs achieved starts/day per lane over the trailing seven-day window … A lane's *configured*
> rate is resolved from its explicit `target_runs_per_day` override, else derived from its durable
> cooldown pin (`86400 / cooldown_pin_s`); *achieved* is the count of ticks that actually spawned inside
> the window, divided by the window length."

and its operator recipe:

```bash
curl -s http://127.0.0.1:9090/api/v1/cadence | jq '.cadence | map(select(.target_deficit_per_day > 0))'
curl -s http://127.0.0.1:9090/api/v1/cadence | jq '[.cadence[].target_source] | group_by(.) | map({source: .[0], lanes: length})'
```

Null hypothesis: *an operator can answer "which lanes are behind their configured cadence, and by how
much" from this endpoint plus its documented recipe, and can act on the answer.*

## 2. What was actually done (real use, not tests)

1. Read §10 as a user, then ran **both** documented recipes verbatim.
2. Used the metric for the thing an operator would use it for: rank lanes by deficit, then explain the
   ranking against the fleet's own admission model and against the DB.
3. Cross-checked **every** served row against `~/.hermes/coding-hermes/scheduler.db`:
   `projects.target_runs_per_day`, `projects.cooldown_pin_s`, `projects.admission_mode`,
   `projects.cooldown_s`, and `ticks.spawned_at` inside the trailing window
   (`/tmp/df-cad-2026-10-20/xcheck.py`, `xcheck2.py`, `gaps.py`).
4. Treated the endpoint as an operator's *first* surface by checking whether the metric appears on the
   dashboard at all (all nine routes), and whether MCP carries it.
5. Measured what the whole thing costs (Step 2b), then profiled only where the number justified it.

## 3. Results

### 3.1 The arithmetic is honest

| Check | Result |
|---|---|
| served rows vs DB project rows | 608 = 608, no missing/extra |
| `target_source` + `target_runs_per_day` recomputed from the documented resolution table | **0 mismatches** |
| `achieved_runs_per_day` recomputed from `ticks.spawned_at` in the window | exact on **606/608**; the 2 differences (`consensus-foreman` 67 vs 68, `qa-audit-foreman` 15 vs 16) are one tick at the moving window boundary |
| NULL honesty | 0 null targets; every lane carries `cooldown_pin_s > 0`, none sets `target_runs_per_day` → `target_source` census = one bucket, `cooldown_pin` × 608 |
| empty response shape | `{"cadence":[]}` on a fresh empty-fleet daemon (§6) — `[]`, never `null`, as documented |

### 3.2 The target denominator is not the lanes' governor — and over-achievement is invisible

`target = 86400 / cooldown_pin_s` for every lane, with **no `admission_mode` term**. But 37 enabled lanes
are `admission_mode='tasks'`, woken by their board through a separate gate
(`internal/scheduler/packer_select.go:376`, `tasksAdmissionDue`), not by the cooldown. For those lanes the
pin is not what governs starts, so the served target and deficit do not answer the operator's question.

Measured over `2026-10-13T07:00Z .. 2026-10-20T07:00Z`:

| Lane | admission | cooldown / pin | served target | achieved | ratio | median tick gap |
|---|---|---|---|---|---|---|
| `consensus-foreman` | tasks | 21600 / 43200 | 2.00/day | 9.71/day | **4.9×** | 3022 s |
| `monitoring-foreman` | tasks | 21600 / 21600 | 4.00/day | 13.29/day | **3.3×** | **2656 s** (82 of 92 gaps *under* its own 21600 s cooldown) |
| `bunker-foreman` | tasks | 21600 / 21600 | 4.00/day | 9.86/day | 2.5× | 5026 s |
| `qa-audit-foreman` | tasks | 21600 / 86400 | 1.00/day | 2.29/day | 2.3× | 20854 s |
| `coding-hermes-scheduler-foreman` | tasks | 21600 / 21600 | 4.00/day | 9.00/day | 2.2× | 3884 s |

22 lanes run above their served target. **Every one of them reports `target_deficit_per_day: 0.0`**,
because the field is documented as `max(0, target − achieved)` — so the endpoint's own recipe
(`select(.target_deficit_per_day > 0)`) can never surface a lane that is over its declared rate, and the
other alarm in this class (`GET /api/v1/cooldown/residue`, 159 rows) cannot either: `monitoring-foreman`
has pin == cooldown (no residue) and `bump_active = 0`.

Fleet-level, the same denominator makes the aggregate unreadable: declared **610.5 starts/day** across
enabled lanes vs **258.7/day** achieved = **42.4%** — a number that looks like "the fleet is starved"
but is really "a pin-derived artifact compared against board-governed lanes". (The 7 fixture lanes whose
pins claim 48–96 starts/day are all `enabled=0`, so the enabled-only number is not fixture-contaminated.)

**Attribution boundary, deliberately kept:** whether the *excess starts* are themselves a defect belongs
to the existing pending rows `SCHED-GAP-1666` ("nudge spawns skip the cooldown today") and
`SCHED-GAP-1660` (the sanctioned tasks-admission exception); the pin/cooldown display residue belongs to
`SCHED-GAP-1744`. This run's finding is the **metric** (`SCHED-GAP-1745`).

### 3.3 The operator cannot see any of this on the dashboard, and the recipe is not actionable

```
for p in / /blocks /health /lanes/tree /observatory /queue /remote /tape /ticks; do
  curl -s -o p.html "http://127.0.0.1:9090$p"; grep -o -i cadence p.html | wc -l; grep -o -i residue p.html | wc -l
done
```

Every route: `0 / 0`, except `/tape`'s 3 `cadence` hits, which are JS strings about the page's own
auto-refresh cadence. So the metric the SOL-CADENCE work exists to expose, and the detector that reds
daily (`SCHED-GAP-1670`/`1744`), are invisible on the dashboard. MCP *does* expose them
(`tools/list` → `cadence`, `cooldown_residue`, 71 tools), so the gap is the UI, not the API.

Recipe 1 (`deficit > 0`) returns **3706 lines / 463 lanes**, ordered by name, headed by seven
`enabled=0` fixture lanes (`dogfood-20260815`, `-dup`, `-guard`, `mythos-foreman`, `zz-gap12-probe`,
`zz-schedgap-011-probe` at 96.00/day from a stray 900 s pin; `dogfood-probe-0925` at 48.00/day) — the
endpoint deliberately serves disabled lanes and offers no `enabled` filter, no sort and no limit. Recipe 2
returns a single bucket (`cooldown_pin` × 608), i.e. nothing. A user asking "which lanes are behind?"
gets 463 alphabetical rows whose top three will never run again → `SCHED-GAP-1746`.

### 3.4 What the control plane costs (Step 2b)

Headline ops, warm, `curl -w '%{time_total}'` (n=3 each unless stated):

| Operation | warm | payload |
|---|---|---|
| `GET /api/v1/cadence` | 10.1 / 10.8 / 11.7 ms | 107,580 B |
| `GET /api/v1/cooldown/residue` | 1.6 / 1.6 / 2.4 ms | 13,433 B |
| `GET /api/v1/health` | 0.42–0.44 ms | small |
| `GET /api/v1/status` | 413 ms | 59,975 B |
| `GET /api/v1/projects` | 42 ms | 742,462 B |
| `GET /` (dashboard root) | **4.3–19 ms** (n=33) | ~135 KB |
| `GET /` cold (after 60 s / 90 s / 15 s idle) | **1.50 / 1.53 / 3.09 s** | ~135 KB |
| `GET /` worst observed (host load 23–32) | **12.03 s** | ~135 KB |

Nothing in the cadence family warrants a perf row — 10 ms for 107 KB is not user-noticeable. The
dashboard's warm path is excellent (4–19 ms); the cold path's seconds-to-tens-of-seconds spread is
already the closed `DASH-PERF-001/003` class, so it is recorded here as a measurement, not re-filed.

What **does** warrant a row is the profile of the daemon itself
(`curl -s -o cpu60.pb.gz 'http://127.0.0.1:9090/debug/pprof/profile?seconds=60'` while driving the
dashboard): **10.97 s of CPU in the 60 s window (18.28% of a core)**, of which

| Frame | CPU | share |
|---|---|---|
| `ReadBoardFreshness` (from `Loop.evaluate → Pack → CountPending → FreshnessVerdictCache.Read`) | 6.05 s | **55.1%** |
| `classifyCompleteRow` | 5.90 s | 53.8% |
| **`internal/scheduler/board_freshness.go:542` alone** (the `for _, other := range ids` mis-attribution scan) | **4.60 s** | **41.9%** |
| the row's own self-match at line 530 | 0.04 s | 0.4% |
| `gitRevVerifyCommit` + `gitReachableFromHEAD` + `gitCommitInfo` (lines 508/514/522) | 0.54 + 0.33 + 0.38 s | 11.4% |
| dashboard `GenerateParams` (`enrichProjects.func1 → readGitReins → os/exec`, `database/sql`) | 1.85 s (1.15 s git + 1.53 s sql) | 16.9% |

The hot path is **quadratic in board size**: for every complete row whose commit does not name it, the
code matches *every* board id against the commit message (`\b<id>\b`). Independent reproduction of the
same algorithm against this repo's real board (`/tmp/df-cad-2026-10-20/repro`):

```
go run . <repo>/.coding-hermes/board/tasks.jsonl <repo>
line 542 quadratic scan: rows=610 x ids=656 = 400160 MatchString calls in 5.130498021s
                         (12.821 us/call, 8.4 ms/row)
```

one full read of *one* 656-row board ≈ **5 s CPU**, matching the 4.60 s in the profile. `PERF-002`
(closed, 332 lanes) found this function family with **git subprocesses** as the hot path; that half is
now ~11%, and the class has recurred with this new dominant mechanism → filed as `PERF-002-R2`.

### 3.5 Install leg

**Bunker leg: SKIPPED-install-bunker.** `bunker-qa.sh` has no host — tailscale: `bunker-las-01` offline
15 d, `-02` offline 7 d, `-03` offline 15 d, `-04` offline 18 d. (The `coding-hermes-remote-bunker-*`
hosts are online but are agent boxes, not bunkerd install targets.) The standing premise is the pending
row `DOGFOOD-025` ("install leg N/A — scheduler is a running daemon … build+API probe is the install
equivalent"), so no duplicate skip row was filed.

**Install-equivalent: RUN, PASSED.** Fresh local clone of `e209c70b` (0 dirty files) → `make build`
**4.10 s wall** (9.84 u + 1.45 s sys; warm Go build cache on this box — the 2026-09-25 fresh-box figure
was 63 s) → `bin/schedulerd` + `bin/migrate` → boot **with no config file** on port 19091 and a scratch
DB → `schedulerd ready` in <1 s:

| Probe (empty fleet) | Result |
|---|---|
| `GET /api/v1/health` | 200, `status:"ok"`, `db:"connected"`, 0.49 ms |
| `GET /` | 200, 39,803 B, 2.08 ms |
| `GET /api/v1/cadence` | `{"cadence":[]}` |
| `GET /api/v1/cooldown/residue` | `{"count":0,"residue":[]}` |
| `GET /api/v1/projects` | `{"projects":[],"total":0,"limit":200,"offset":0}` |
| boot banner | `Auth: FAIL-CLOSED — no operator credential configured; every mutating route answers 503` |

First-boot friction (recorded, not filed as a row): the zero-config daemon still enables DuckBrain sync
against `localhost:3000` and logs five `duckbrain key rejected (HTTP 401)` lines plus a queued HIGH event
against a service the new user never configured, while `docs/reference/flags.md` documents no way to
switch sync off (only `--duckbrain-url`, `--duckbrain-ns`, `--duckbrain-interval`). The scratch daemon was
killed by port and the port verified free (`fuser -k 19091/tcp`; zombie reaped by its parent).

## 4. Verdict

**🟡 PROMISING-BUT-ROUGH.** The cadence surface answers fast, honestly and self-consistently at the
arithmetic level, and its docs describe the shape accurately. What it does not yet do is answer the
question it exists for: for the lanes whose wake is board-driven, the served target is a cooldown-pin
artifact rather than an intent, the deficit clamp removes the over-target direction entirely, and the
operator cannot see the metric on the dashboard. The control plane, meanwhile, pays ~5 s of CPU per board
read in a single quadratic regexp loop on the pick path.

**Friction count 3:** unusable documented recipe; metric invisible on the operator's own surface; missing
admission-mode context (the operator cannot tell which lanes the target even binds).

## 5. Reproduce everything

```bash
# served metric + recipes
curl -s http://127.0.0.1:9090/api/v1/cadence | jq '[.cadence[]|select(.target_deficit_per_day>0)]|length'   # 463
curl -s http://127.0.0.1:9090/api/v1/cadence | jq '[.cadence[].target_source]|group_by(.)|map({s:.[0],n:length})'
# ground truth
sqlite3 'file:/home/kara/.hermes/coding-hermes/scheduler.db?mode=ro' \
  "SELECT admission_mode,COUNT(*) FROM projects WHERE enabled=1 GROUP BY admission_mode;"   # cooldown 370, tasks 37, '' 4
# dashboard absence
for p in / /blocks /health /lanes/tree /observatory /queue /remote /tape /ticks; do curl -s "http://127.0.0.1:9090$p" | grep -ci cadence; done
# control-plane cost
curl -s -o cpu60.pb.gz 'http://127.0.0.1:9090/debug/pprof/profile?seconds=60'
go tool pprof -list=classifyCompleteRow cpu60.pb.gz | sed -n '/542:/p'      # 4.60s on the loop line
cd /tmp/df-cad-2026-10-20/repro && go run . <board>/tasks.jsonl <repo>       # 400,160 matches in 5.13s
# cold/warm dashboard
sleep 60; curl -s -o /dev/null -w 'cold %{time_total}\n' http://127.0.0.1:9090/; curl -s -o /dev/null -w 'warm %{time_total}\n' http://127.0.0.1:9090/
```

## 6. Diagnostic trail (how this was built, and what misled me first)

- **The first number was a lie of circumstance.** The first `GET /` I timed was **13.5 s**; it looked
  like a headline regression until a controlled cold/warm sweep (`/tmp/df-cad-2026-10-20/cold_probe.sh`)
  showed 1.50 s / 1.53 s / 3.09 s cold and 4.3–19 ms warm. The 13.5 s sample happened under host load
  ~32 with the fleet busy — the honest summary is the *spread*, not the peak. Same discipline on the
  served-vs-DB comparison: two `achieved` mismatches (`consensus-foreman`, `qa-audit-foreman`) turned out
  to be exactly one tick at the moving window boundary, so they were **not** filed.
- **My first hypothesis about the over-achievers was wrong, and the measurement refuted it.** I assumed
  the excess came from `cooldown_s` sitting below the pin (the `SCHED-GAP-1744` residue) — but
  `monitoring-foreman` has pin == cooldown == 21600 s and still spawns with a 2656 s median gap, and
  `SCHED-GAP-1744`'s own reasoning shows the packer *honours* the pin (`loop.go:2298`,
  `packer_select.go:342`). The mechanism is the separate tasks-admission gate
  (`packer_select.go:376`), which is why the finding is scoped to the **metric** and the excess-starts
  question was left to the rows that already own it.
- **Check coverage before claiming a gap.** MCP turned out to expose both `cadence` and
  `cooldown_residue`; the served OpenAPI includes both paths. Only the dashboard and the recipe are
  actually missing → the row says exactly that instead of "the feature is invisible".
- **`grep -c` on a rendered page can lie about page content.** `/tape` first reported 3 `cadence` hits
  that vanished on a re-fetch; the surviving explanation is the page's own auto-refresh JS strings
  (confirmed by context-grepping the hits). Any "metric is missing from the UI" claim in this report is
  based on a fresh fetch per route, printed in §3.3.
- **Unattended-surface mechanics that cost time:** `sh -c '…'` and a foreground command containing `&`
  are both screened (launch wrappers must be a `bash <script>` background call), `python3 -c` is
  screened (scripts via `write_file` + `python3 file`), and `ls /etc/<config>` inside an otherwise
  harmless command trips the "system config path" screen. The board writes go through
  `~/.hermes/scripts/board_append.py` (append-only, re-read verified): `tasks.jsonl 656 → 660`,
  `events.jsonl 1261 → 1262` (audit event `1792220000000000148`), then `boardctl validate` → `RESULT: OK`.
