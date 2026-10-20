# Dogfood — the operator troubleshooting taxonomy and the drain/restart pre-flight, driven for real

**Run:** tick `coding-hermes-scheduler-dogfood-2026-10-20-06-54-47`, **second angle** (run 2 of the tick).
Run 1 of the same tick landed first (`784fdd8a` cadence read surface + `PERF-002-R2`; `493d11c2`; `e68834f0` second write + `PERF-001-R2`).
**Target:** `/home/kara/coding-hermes-scheduler/coding-herms-scheduler`, HEAD `e209c70b` at the board read (live daemon `v1.12.0-488-gd6e17be8`, uptime 2 h 44 m, 608 projects / 411 enabled, host load 9.8–25.0).
**Verdict:** 🟡 **PROMISING-BUT-ROUGH — the taxonomy is right far more often than it is wrong, but when the daemon is busy it reports the opposite of the truth, and the operator has no way to tell.**

---

## 1. What was driven (and why this angle)

Prior runs covered the control surface (09-25), the dashboard at scale (09-25), the simulator (09-25), the federation read surface (10-17), and — run 1 of this same tick — the cadence read surface. Uncovered: **the operator's diagnostic path**, which is what the product's second README promise ("Exposes control — REST API, MCP, dashboard…") is *for*.

Two documented, never-driven operator surfaces:

| Surface | Claim it makes |
|---|---|
| `docs/troubleshooting-scheduling-errors.md` (1120 lines) | "Audience: an operator who has never read the scheduler's source. Every entry is `Symptom → Means → Confirm → Fix`, and every confirm is ONE copy-pasteable command whose expected output is quoted. No entry requires reading Go." |
| `docs/runbook-drain-restart.md` §1–§3, §5–§7 (read-only legs) | Pre-flight ⇒ pause ⇒ drain ⇒ restart ⇒ verify ⇒ resume, with "the two ops gates" at §1.6 and an exit-code contract (0 FRESH / 1 STALE / 2 UNKNOWN = a deploy failure). |

**Method.** Every fenced `sh` block in the taxonomy was extracted programmatically (71 blocks; 34 read-only confirms selected across all 12 sections), written to its own script, and run verbatim against the live daemon with rc + wall time + raw output captured per block (`/tmp/df-sched-1020/confirms/`, driver `/tmp/df-sched-1020/drive_confirms.py`). Nothing mutating was run — `pause`/`resume`/`evaluate` were read-only-verified in the page and deliberately left unexecuted, exactly as the page itself tags them.

**Not driven, and why:** the drain/restart's mutating steps (pause → restart → resume) are fleet-wide state changes and out of scope for a read-only dogfood lane; run 1 of this tick did the install-equivalent leg (fresh local clone + `make build` + zero-config boot smoke on :19091) and `SKIPPED-install-bunker` (all four `bunker-las-*` tailnet hosts offline), so this run inherited that proof rather than re-spawning an unchanged HEAD (see `docs/dogfood/2026-10-20-cadence-cost-and-install.md`).

## 2. What held up (evidence, so the next run does not re-prove it)

30 of 34 confirms matched the page's quoted shape, and where the page dates a numeric quote the values still matched:

- §1 "503 drain responses are booked as lane failures": `245 failed-or-timed-out 7d / 52 were drain-503` — **exactly** the quoted pair; the per-reason split (`gateway_transport 138`, lane-attributed 106, `instant_turn 1`) explains the symptom the entry is attached to.
- §5 cap-vs-load-gate: `ADMIT pass_id=42 eligible=402 admitted=0 deferred=402 … reason=load_gate load1=23.88 gate=20.0` — the documented marker, prefix-free, exactly as §0 describes.
- §6 stale queued: `queued_rows 0 / stale_over_4h 0`. §11 slot expiry: a real `slot wait expired — dropped duckbrain-pm` line. §12 armed auto-disable: `{"armed":2,"names":["sim-beta","sim-delta"]}` — the page's example, still live.
- §0's surface table is accurate, including its subtle warning that `ADMIT` is the only prefix-free marker.

## 3. What broke — four findings

### 3.1 `PERF-001-R3` (P1) — the read path waits on the loop's write lock, and the 5 s budget cannot bound it

The page's §9/§10 confirm (`curl -s … /api/v1/status | jq …`) took **7.488 s** and **17.287 s** on this tick's first operator actions, and the daemon logged both:

```
WARN: slow request handler=status step=configDriftBlock elapsed=17.511s \
      slowest_step=runningWaveCostTotal slowest_step_elapsed=16.81s budget=5s
```

A goroutine dump taken **during** a 21.8 s request (which returned `504 {"error":"deadline exceeded","helper":"configDriftBlock",…}`) shows the reader and the holder in one picture:

- **reader** — `goroutine 1983047 [sync.RWMutex.RLock]` → `internal/scheduler.(*Loop).ZeroSelectStats` (`internal/scheduler/loop.go:1746`) ← `internal/api.(*Server).status` (`internal/api/server.go:656`);
- **holder** — `goroutine 195753 [runnable]` → `syscall.forkExec` ← `gitCommitInfo` ← `classifyCompleteRow` ← `ReadBoardFreshness` ← `PendingTaskCounter.cachedFreshnessRead` ← `(*Loop).evaluate` (`internal/scheduler/tick_process.go:174`).

So `--api-read-timeout=5s` (live: `{"api_read_timeout":"5s"}`) does not bound the caller's wall time: the budget is a context deadline checked *between* handler steps, and a `sync.RWMutex.RLock` wait (and the DB steps behind the single shared connection, `internal/database/schema.go:45`) cannot observe cancellation. The 504 is *delivered* 13.9–21.8 s late — 3–4× the armed budget — and names `configDriftBlock` while the slowest step was `runningWaveCostTotal`. The 16.8 s is not SQL: the same query read-only against a copy of the live DB runs in 0.00 s (12/12 warm runs, `EXPLAIN QUERY PLAN` → `SEARCH t USING INDEX idx_ticks_status_running`), and `/debug/pprof/{block,mutex}` both return **zero samples** (207/208 B) — the wait is invisible to the process profiles, which is exactly what run 1's `PERF-001-R2` reported and why it asked for the eval-lock A/B.

**Warm/cold for the headline operation** (`/api/v1/status`, 59,975 B):

| | value |
|---|---|
| warm n=40 | 39/40 = 200 in **0.311–0.494 s**; 1/40 = **504 after 13.891 s** |
| cold (90 s idle) | **15.946 s, HTTP 504** — the next call 0.535 s, HTTP 200 |
| n=80 repeat | 1/80 = 504 after 16.498 s |
| stall catcher | 1/47 = 504 after 21.820 s |
| operator-visible rate | 3 of 6 back-to-back operator runs saw `/status` exceed 10 s; one exceeded 60 s |

### 3.2 `SCHED-GAP-1747` (P2) — the runbook's "ops gate" reports a healthy daemon as DOWN

`ops/check-daemon-freshness.sh` (runbook §1.6, whose exit 2 the runbook calls "a failure for deploy purposes") returned, on first invocation:

```
check-daemon-freshness: no JSON response from http://127.0.0.1:9090/api/v1/status — daemon down or wrong endpoint
UNKNOWN: live=<no response> reference=67256419 … EXIT=2
```

while `/api/v1/health` answered **200 in 0.54 ms** and `/api/v1/status` answered 200 in 0.50 s a minute later. The script hard-codes its ceiling at `ops/check-daemon-freshness.sh:163` (`curl -fsS --max-time 10`), so §3.1's stall is read as an outage. A/B on the same URL, same minute, 6 rounds: stock 10 s copy returned rc=2 three times in a row (10.02/10.03/10.03 s) where a 60 s-timeout copy still failed twice (24.4 s, 60.0 s) and then 8/8 stock runs returned **rc=0 FRESH in 0.50–1.30 s**. The gate is time-correlated with the stall, not with daemon state — and it loses signal in both directions (a real stop is indistinguishable from a slow read).

### 3.3 `SCHED-GAP-1748` (P3) — the board-invariant checker floods one file-level condition

§7's documented confirm on the consensus board: **651 output lines, 647 violations, 643 of them the same `event-id-ascending` line** for one property of one file, which the tool's own INFO line already states once — and it prints that INFO *after* the flood, with the 4 real findings (`board-legacy-status`) buried and no `board-content-dup` verdict at all (the page quotes one). The same command on this repo's own board prints 4 lines and PASS, so the flood is shape-conditional and hits precisely the board an operator runs it against while diagnosing.

### 3.4 `DOC-22` (P3) — the page's confirms return a confident false negative

- **A 504 renders as a clean reading.** `curl -s` (no `-f`) plus jq's null-for-missing prints the 95-byte failure body as `{"zero_select_consecutive":null,"zero_select_eligible":null,"zero_select_last_at":null}` — indistinguishable from a reading, and the reassuring reading at that ("never fired"), on the page's own zero-select watchdog. Both runs of that documented command printed exactly that.
- **The log is rotated and the page says it is durable.** `/etc/logrotate.d/hermes` rotates `~/.hermes/coding-hermes/*.log` with `size 50M, rotate 3, copytruncate`; the live file covers 2 h 10 m while `.1` (87 MB) / `.2.gz` / `.3.gz` hold the rest. Three confirms in the page therefore return *empty* and read as "never happened": `EVAL-ZERO-SELECT` 0 hits (page quotes a real line), `RESUME: deferring orphaned tick.*at cap` 0 (page quotes 25), `QUEUED: reaped` empty (page quotes a line). All three markers still exist in the code (13 / 5 / 1 grep hits), so these are window artifacts.

## 4. Value judgement (Step 3)

1. **Does it work?** Yes — 30/34 documented confirms behaved as written, and the four that did not fail *in the direction of false comfort*, which is the honest summary.
2. **Is it useful?** Yes. This is the page an on-call operator (human or agent) reaches for first, and its `Symptom → Means → Confirm → Fix` discipline is real; the `ADMIT` marker, the three-surfaces table and the exit-code contract are the kind of thing most projects never write.
3. **Is it usable?** Time-to-first-answer ≈ 30 s (one `curl`). The friction is not finding the command, it is believing the result: twice this run the page's own command produced a confident wrong answer.
4. **Is it trustworthy?** Partly. Nothing was corrupted and the read-only commands mutate nothing; but a false "daemon down" during a deploy window and a false "zero-select: never" are exactly the two readings that make an operator stop looking.
5. **Is it fast enough?** Warm `/status` (0.31–0.49 s) and `/health` (0.5 ms) are comfortably fast; the 7–60 s stalls are not a "fast enough" question but the P1 above — a user *cannot* miss them (3/6 operator runs).

**Friction count: 3** (false "daemon down" on the runbook gate; null-vs-504 ambiguity on two confirms; log-grep window with no rotation note). **Cost of the run:** ~55 min wall, of which ~20 min was the stall attribution.

## 5. Traceability and the catalog gap

`ch:trace` (per `coding-hermes-traceability`):
`row=PERF-001-R3 spec=docs/reference/flags.md:27,docs/api.md:378-382 test=none:manual-procedure(dogfood §2/§3) evidence=/tmp/df-sched-1020/{status-loop.tsv,stall/stall47.goroutine.txt,confirms/} witness=live:goroutine-dump-mid-504+daemon-WARN-lines verdict=recurrence-of-PERF-002-R2 arm=read-path-starvation commit=none:row-not-dispatched`

**Catalog gap (recorded, not guessed):** the run history the dogfood/QA skill expects to key findings against — `docs/qa/use-cases.md` (or `docs/qa/run-history.jsonl`) — **does not exist in this repo** (`docs/qa/` is absent; the closest thing is `docs/verification/` with two one-off pages). Findings here therefore carry ad-hoc `REG-…`-less evidence and the canonical history is `.coding-hermes/dogfood-log.md`, which is what this run used. Whether this page's confirms should become permanent regression cases (a `REG-*` row per confirm, re-driven each dogfood pass) is a decision for the owning foreman; the cheapest shape is a single scripted battery over the page's read-only blocks, which this run already built (`drive_confirms.py` + the JSON block extractor).

## 6. Artifacts

- Board rows: `PERF-001-R3` (P1), `SCHED-GAP-1747` (P2), `SCHED-GAP-1748` (P3), `DOC-22` (P3) — filed with `boardctl create`; `boardctl validate` → `RESULT: OK`.
- Scratch evidence (kept): `/tmp/df-sched-1020/` — `trouble-blocks.json` (71 extracted blocks), `confirms/L*.sh|.out` (34 blocks: rc, wall, raw output), `TIMINGS.txt`, `status-loop.tsv` (n=40), `stall/stall47.goroutine.txt` (the mid-stall dump), `freshness/{stock.sh,patched.sh}` + A/B transcript, `hotpath.sql` (EXPLAIN QUERY PLAN + row counts), `inv-consensus.out` (the 651-line flood).
- Read-only lane: no source, no scheduler state, no fleet config, no repo visibility and no credential was touched; the only writes are this file, the log entry below, and the four board rows.
