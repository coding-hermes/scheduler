# Troubleshooting — scheduling errors, by symptom

**Applies to:** `coding-hermes-scheduler` (the `schedulerd` daemon) on the fleet host.
**Audience:** an operator who has never read the scheduler's source. Every entry is
`Symptom → Means → Confirm → Fix`, and every confirm is ONE copy-pasteable command whose
expected output is quoted. No entry requires reading Go.
**Owner:** `coding-hermes-scheduler` project. **Related:** SCHED-GAP-153 (this taxonomy),
SCHED-GAP-125/171 (load gate), SCHED-GAP-142/146 (orphan nudges + the gates they must honor),
SCHED-GAP-143 (transport-class failure stamping), SCHED-GAP-145 (stale `queued` reap),
SCHED-GAP-148/162 (daemon freshness + the open deploy gap), SCHED-GAP-155 (the `ADMIT` line),
SCHED-GAP-164/172 (board vocabulary + content duplicates), GAP-042/043 (eval-stall, zero-select).

**This page is the taxonomy. `docs/runbook-drain-restart.md` is the procedure** — pause, drain,
restart, verify, resume. Where the runbook already owns a procedure this page links to its
anchor instead of restating it. Where this page says "see the runbook §8 Trap B", `§8 Trap B`
is a real anchor in that file.

---

## 0. How to read this page

Every command is tagged:

| Tag | Meaning |
|-----|---------|
| *(read-only)* | Safe to run against production at any time. **Executed while authoring this page (2026-09-20) at HEAD `19e9fd6d`; the real output is quoted.** |
| *(not executed)* | Live-state-dependent, mutating, or requiring a drain that the author did not perform. The expected **shape** is stated. Nobody executed these to write this page. |

The commands are of three kinds and nothing else: `curl` against the loopback API, `sqlite3`
read-only SQL, and `grep` over the daemon log or a board file. If you can run `curl`, `sqlite3`
and `grep`, you can diagnose every entry below.

**Do not write to the live database to diagnose anything.** Every query on this page is either a
plain `SELECT` or uses `sqlite3 -readonly` / `sqlite3 "file:…?mode=ro"`. The runbook states the
same rule; the only writes in this system belong to the daemon and to the sanctioned ops scripts.

Two abbreviations used throughout: **the DB** is the SQLite file, **the API** is the loopback
daemon.

---

## The surfaces — where state actually lives

Three places, and each one answers a different question. If a symptom is not visible on one,
check the other two before concluding anything.

| # | Surface | Path / address | What it is authoritative for |
|---|---------|----------------|------------------------------|
| 1 | **SQLite DB** | `~/.hermes/coding-hermes/scheduler.db` (the `--db` default) | The durable record: `ticks`, `events`, `projects`, `namespaces`. Survives restarts. This is the only place a *terminal* fact (a tick's outcome, an orphan reason, a failure class) actually lives. |
| 2 | **Daemon API** | `http://127.0.0.1:9090/api/v1/…` — loopback, **unauthenticated**, read-preferred | Live in-process state the DB cannot hold: `paused`, per-process counters that reset on restart (`admission_counters`, `gateway_errors`, `spawns_http`), and computed views (`active_ticks`, `projects_failure_rates`). Key routes: `/status`, `/ticks`, `/events`, `/queue`, `/health`, `/config`, `/metrics`. |
| 3 | **Daemon log** | `~/.hermes/coding-hermes/scheduler.log` (the `--log-file` default), plus `journalctl --user -u coding-hermes-scheduler.service` | The narrative: one line per decision. Grep-able markers: `EVAL:`, `ADMIT `, `EVAL-STALL:`, `EVAL-ZERO-SELECT:`, `SPAWN:`, `SLOT:`, `RESUME:`, `QUEUED:`, `DANGLING:`, `CLEANUP:`, `FAIRNESS:`, `TIME: clock`. |

Three facts about the surfaces that cost time if you learn them the hard way:

- **The log file is appended to by the *previous* run too.** `scheduler.log` is durable across
  restarts; `journalctl` for the user unit is not (the unit's journal held only the current
  boot during authoring — 3,294 lines, starting 08:23:53, while the daemon itself had started
  21 hours earlier). A `grep -c` over the file counts history; a `grep -c` over the journal
  counts this boot. Say which one you mean.
- **The `ADMIT ` marker is prefix-free and the other markers are not.** `ADMIT` lines are
  written straight to the log writer with no timestamp prefix, so `grep -E '^ADMIT '` matches
  them. Every other marker (`EVAL:`, `RESUME:`, …) is prefixed with
  `2026/09/20 08:31:08 file.go:150: `, so anchor those on the token, not on the line start.
- **Per-process counters reset on restart.** `admission_counters.passes` and `gateway_errors`
  are in-memory; the `ADMIT` log line's `pass_id` restarts at 1. A counter that looks small is
  usually just young — compare it against `passes`, not against the DB's row counts.

Boot-time sanity, useful before any of the entries below:

```sh
curl -s --max-time 20 http://127.0.0.1:9090/api/v1/health
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```json
{"active_ticks":9,"build_sha":"1f8e95e","build_time":"2026-09-19T15:46:11Z","db":"connected","evaluation_age_seconds":53.493707954,"gateway_errors":25,"last_evaluation":"2026-09-20T13:28:14Z","spawns_exec":0,"spawns_http":295,"status":"ok","uptime":"21h42m54.182324747s","version":"v1.3.0-161-g1f8e95e-dirty"}
```

`build_sha` is the field that matters (entry 8). If `curl` hangs rather than failing, raise
`--max-time`: a heavily loaded daemon was observed taking 8.3 s on the first `/api/v1/status`
call in a burst and 0.2–0.3 s immediately after. A hang is not proof the daemon is down.

---

## 1. 503 drain responses are booked as *lane* failures

**Symptom** — Per-project failure rates climb across unrelated lanes at the same moment. The
events table fills with `HIGH` rows from component `spawn` reading `gateway spawn dropped`. An
operator concludes a dozen projects went bad; the projects are fine.

**Means** — The Hermes gateway refuses spawns while it drains, answering
`HTTP 503 … Gateway is draining existing work; retry shortly.` The scheduler's spawn path books
that as the *project's* failed tick. It is a **harness** failure — every project on the box
fails identically — and it must not feed per-project health accounting. The classifier that
decides this is a single shared list of markers, and a failed row carries the verdict in
`ticks.failure_reason` (`gateway_drain` for a drain refusal, `gateway_transport` for every other
harness-side class, empty for a genuine project-side failure). Legacy rows written before that
stamping existed have `failure_reason = ''` and must be classified from `ticks.error` text.

**Confirm** *(read-only)*

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" "
SELECT COUNT(*) AS failed_or_timedout_7d,
       SUM(lower(coalesce(error,'')) LIKE '%503%') AS rows_that_were_drain_503
FROM ticks WHERE status IN ('failed','timeout') AND created_at >= datetime('now','-7 days');
SELECT CASE WHEN failure_reason='' THEN '(none — lane-attributed)' ELSE failure_reason END AS failure_reason,
       COUNT(*) AS n
FROM ticks WHERE status IN ('failed','timeout') AND created_at >= datetime('now','-7 days') GROUP BY 1 ORDER BY n DESC;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
failed_or_timedout_7d  rows_that_were_drain_503
---------------------  ------------------------
                  915                       797
     failure_reason        n
------------------------  ---
(none — lane-attributed)  909
gateway_transport           6
```

797 of 915 failures (87%) were gateway 503 drain responses. The second table is the part that
matters: only 6 rows carry a `failure_reason` stamp, because 909 of them were written **before
the current daemon started** (2026-09-19T10:46 local). The stamping itself is live in the running
build — you can prove it from the same data rather than trusting the source:

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT COUNT(*) AS gateway_fails_since_daemon_start,
        SUM(CASE WHEN failure_reason <> '' THEN 1 ELSE 0 END) AS stamped
 FROM ticks WHERE status IN ('failed','timeout')
   AND lower(coalesce(error,'')) LIKE '%gateway%'
   AND julianday(created_at) >= julianday('2026-09-19T10:46:12-05:00');"
```

EXPECTED OUTPUT *(real, 2026-09-20 — the daemon-start instant is the `lstart` you recorded)*

```text
gateway_fails_since_daemon_start  stamped
--------------------------------  -------
                               6        6
```

6 of 6. Every gateway-text failure since the current daemon came up carries the classification,
and none before it do. The honest reading of the first table is therefore **not** "909 lanes are
broken" and not "the fix is missing" — it is "909 rows were written by an older binary and must
be classified from their error text, as the query above does."

The single row behind one of those counts, verbatim:

```sh
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT status, outcome, exit_code, failure_reason, error FROM ticks
 WHERE lower(coalesce(error,'')) LIKE '%503%' ORDER BY created_at DESC LIMIT 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
failed|failed|0||gateway unreachable and exec fallback disabled: gateway POST: HTTP 503: invalid_request_error: Gateway is draining existing work; retry shortly.
```

Read `error` for the four words that classify it: **`Gateway is draining`**.

**Fix** — Nothing to fix in the project. Two things to know:

- The gate now **defers instead of failing** — a draining or unreachable gateway causes the
  spawn to be skipped before any tick row is created (no slot, no pick, no cooldown, no booked
  failure), and the deferral is *reported*: a `gateway` event names the project and the tick id.
  Confirming this class by hand is how you tell "the fix is deployed" from "the fix is merged"
  (entry 8). Note the two halves travel separately: the classifier and the deferral are distinct
  commits, so a daemon can have the first and not the second. Check the classifier first (the
  query above — it is the cheap one), then look for `gateway` events in the `events` table.
- If the gateway is genuinely draining because of a **scheduler restart**, that is the runbook's
  territory — the drain is a deliberate step. Go to the runbook's drain and restart sections.
  If it drains with no operator in the middle of a runbook, that is the runbook's §8 Trap D
  (a parked fleet) or an upstream restart; check `paused` before assuming the scheduler is at
  fault.

---

## 2. Orphan re-nudges burst past a namespace cap at startup

**Symptom** — Immediately after a restart, a namespace with `max_concurrent = 1` shows several of
its lanes running at once, or lanes from an unrelated namespace never get a slot. The log shows a
burst of `RESUME: nudged orphaned tick …` lines within the same second.

**Means** — The orphan re-nudge path is **not** the packer. On startup (and when the gateway
comes back) the scheduler scans for ticks orphaned by the previous process and re-queues each as
`<original-id>-nudge<N>`. That path spawns directly, so it has to consult every admission gate
itself or it bypasses them. Two gates are relevant and both are now checked *before* a nudge is
enqueued, so a deferred candidate consumes **nothing** — no nudge, no `queued` row, no slot:
the namespace cap, and the load gate. Each tick row may be nudged at most
`MaxNudgesPerTick = 2` times; beyond that it is failed into `needs-human` via a `HIGH` event
rather than resurrected again (anti-zombie-resurrection).

**Confirm** *(read-only)*

```sh
LOG="$HOME/.hermes/coding-hermes/scheduler.log"
grep -c 'RESUME: deferring orphaned tick.*at cap' "$LOG"
grep -m1 'RESUME: deferring orphaned tick.*at cap' "$LOG"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
25
2026/09/18 04:23:10 session_resume.go:209: RESUME: deferring orphaned tick warpfs-2026-09-17-06-00-01 (project warpfs) — namespace coding-hermes at cap 8 (4 in flight, 4 admitted this pass)
```

A `deferring … at cap` line is the **healthy** shape: the cap held. Its absence plus multiple
nudges in one second is the burst. The second half of the contract — the cap reached but the
work still visible — is confirmed from the other side:

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT severity, COUNT(*) AS n FROM events WHERE component='resume' GROUP BY 1;"
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT severity, COUNT(*) AS n FROM events WHERE component='resume' AND message LIKE '%needs human%' GROUP BY 1;"
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT message FROM events WHERE component='resume' ORDER BY id DESC LIMIT 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
severity   n
--------  ---
HIGH      221
severity   n
--------  ---
HIGH      163
orphaned tick needs human — project eduos-sync tick eduos-sync-2026-09-17-02-13-12 not resumed (nudge cap 2/2 reached)
```

Read the two counts together: **221** resume events total, of which **163** are `needs-human`
drops and the remaining 58 are successful re-nudges (`RESUME: nudged orphaned tick …` — the same
58 the log grep reports). The `needs-human` message is the terminal state of a tick that
exhausted its nudges. It is a `HIGH` event by design: a drop must be visible, never silent.

The nudge budget itself, from the DB:

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT nudge_count AS nudges_consumed, COUNT(*) AS tick_rows
 FROM ticks WHERE orphaned_at IS NOT NULL GROUP BY 1 ORDER BY 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
nudges_consumed  tick_rows
---------------  ---------
              0         12
              1          6
              2         26
```

`nudges_consumed` never exceeds 2 — the cap is a schema-level fact you can see here.

**Fix** — If you see a real burst (several nudges admitted into one capped namespace in one
pass), that is the SCHED-GAP-142 class: the nudge path is not honoring the cap. Do **not**
hand-edit `nudge_count` or delete nudge rows — that removes the audit trail and the cap will
re-burst on the next restart. File it with the `RESUME:` lines and the count above as evidence.
If nudges are being **deferred** (`at cap`, `load gate active`) that is correct behavior and
needs no action: the work is re-picked once a slot frees or load drops. A tick sitting at
`needs-human` is an intentional stop; the runbook's restart procedure is the context in which
these appear.

---

## 3. Ticks killed by a drain

**Symptom** — A restart completes, the fleet looks healthy, and a handful of ticks from the
minutes before the restart read `failed` with `exit_code = 0` and the error
`aborted by graceful shutdown — drain timed out with tick in flight`.

**Means** — The daemon was stopped while ticks were still running and the graceful-shutdown drain
window expired before they finished. Those rows are **not** the project's fault, and the fact is
recorded separately from the failure: `ticks.orphaned_at` + `ticks.orphan_reason` are stamped,
with `drain_timeout` for exactly this path and `zombie_reap` for a live-daemon tick whose
pid/heartbeat died. The tick timeout is `7200s` (2 h), so a drain can legitimately take that
long. Marks the drain as a deliberate operator step, not a crash.

**Confirm** *(read-only)*

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT orphan_reason, COUNT(*) AS n, MIN(SUBSTR(orphaned_at,1,10)) AS first, MAX(SUBSTR(orphaned_at,1,10)) AS last
 FROM ticks WHERE orphaned_at IS NOT NULL GROUP BY 1 ORDER BY n DESC;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
orphan_reason  n     first        last
-------------  --  ----------  ----------
drain_timeout  27  2026-09-03  2026-09-18
zombie_reap    17  2026-09-03  2026-09-16
```

Drill into a drain victim and its error text:

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT SUBSTR(id,1,40) AS id, status, outcome, exit_code, orphan_reason
 FROM ticks WHERE orphan_reason='drain_timeout' ORDER BY created_at DESC LIMIT 3;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
                  id                     status  outcome  exit_code  orphan_reason
---------------------------------------  ------  -------  ---------  -------------
coding-hermes-tools-2026-09-18-09-04-52  failed  failed           0  drain_timeout
warpfs-2026-09-18-09-03-50               failed  failed           0  drain_timeout
off-by-one-2026-09-18-08-55-58           failed  failed           0  drain_timeout
```

```sh
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT SUBSTR(error,1,90) FROM ticks WHERE orphan_reason='drain_timeout' ORDER BY created_at DESC LIMIT 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
aborted by graceful shutdown — drain timed out with tick in flight
```

**Fix** — Per-tick: nothing — these rows are correctly classified and the orphan scan re-nudges
them if the work stalled (entry 2). Per-process: the correct response is to make the next drain
cleaner, which is a procedure, not a diagnosis. **See `docs/runbook-drain-restart.md` §3
(drain — wait for `running = 0`) and §8 Trap A** (a drain helper killed mid-way leaves the whole
fleet paused). Do not delete or re-status these rows: `orphaned_at` is what the resume scan keys
on, and rewriting it silently disables the recovery path.

---

## 4. Spawn failure vs timeout — they are different outcomes

**Symptom** — Two `failed`-looking ticks that behave nothing alike: one failed 3 seconds after it
started and one ran for two hours. Both read `failed`/`timeout` somewhere in a dashboard, and an
operator cannot tell "the harness refused to start this" from "this ran and the project timed
out."

**Means** — They are distinct states with distinct evidence, all on the same row:

| Column | `failed` (a spawn/session failure) | `timeout` (ran, then died on the deadline) |
|--------|-----------------------------------|--------------------------------------------|
| `ticks.status` | `failed` | `timeout` |
| `ticks.outcome` | `failed` when a session was reached; `NULL`/empty when it never was | `NULL`/empty for a reaper-driven timeout |
| `ticks.exit_code` | `NULL` when no process exit exists (a gateway-refused spawn, a graceful-shutdown drain — SCHED-GAP-1597: no longer a fabricated `0`), `0` on a **completed** gateway tick (the stated convention — the gateway session exposes no process exit), a real process code (e.g. `2`) when a child process ran and exited non-zero | usually `NULL` |
| `ticks.error` | the refusal text, e.g. `gateway unreachable … HTTP 503 …` | a deadline text, e.g. `stale — timeout at 1h30m0s` |
| `ticks.failure_reason` | `gateway_drain` / `gateway_transport` for harness-side; empty otherwise | empty |

The trap: `exit_code = 0` on a **completed** gateway row does not mean "a process exited
cleanly" — the gateway session never exposes a process exit, and `0` is the column's stated
completion convention (SCHED-GAP-1597). On a failed row, a `0` after 2026-09-24 is a bug (the
convention for "no process exit" is `NULL`); before that date, `0` on a gateway-refused spawn
meant the same absence rather than a clean exit. Either way, distinguish by reading `error`,
not by reading `exit_code` alone.

**Confirm** *(read-only)*

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT status,
        CASE WHEN outcome IS NULL OR outcome='' THEN '(none)' ELSE outcome END AS outcome,
        CASE WHEN exit_code IS NULL THEN 'NULL' ELSE CAST(exit_code AS TEXT) END AS exit_code,
        SUBSTR(REPLACE(COALESCE(error,'(none)'),CHAR(10),' '),1,38) AS error_prefix,
        COUNT(*) AS n
 FROM ticks WHERE created_at >= datetime('now','-7 days') AND status IN ('failed','timeout')
 GROUP BY 1,2,3,4 ORDER BY n DESC;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
status   outcome  exit_code               error_prefix                n
-------  -------  ---------  --------------------------------------  ---
failed   failed   0          gateway unreachable and exec fallback   831
failed   failed   NULL       stalled: no progress for 30m0s (gatewa   40
failed   failed   0          aborted by graceful shutdown — drain t   19
timeout  (none)   NULL       (none)                                   15
failed   (none)   NULL       stale queued row — no live process at     7
failed   failed   2          exit status 2                             1
failed   failed   NULL       gateway response failed (len=37): Time    1
timeout  (none)   NULL       stale — timeout at 1h30m0s                1
```

Read the rows as five different stories, one line each:

- `failed` / `NULL` / `gateway unreachable … 503` — **harness refused the spawn.** No
  process ran (SCHED-GAP-1597: no-process failures persist `NULL`; rows before
  2026-09-24 show the fabricated `0`). Not the project's fault (entry 1).
- `failed` / `NULL` / `stalled: no progress for 30m0s` — **a real session that stopped
  reporting** inside its per-turn deadline. The tick budget was still alive; this is the
  turn-level deadline, not the tick-level one.
- `failed` / `NULL` / `aborted by graceful shutdown — drain timed out` — **an operator
  drain** (entry 3; SCHED-GAP-1597: the drain reap no longer writes a fabricated `0`).
- `timeout` / `NULL` / `(none)` — **no error text at all**, which is the shape of a tick reaped
  by the reaper rather than one that failed on its own.
- `failed` / `exit_code 2` / `exit status 2` — **a child process ran and exited 2.** This is the
  only row in the window that is unambiguously a project-side non-zero exit.

**Fix** — Classify first, then act:

- Harness-side rows (`gateway…`, `aborted by graceful shutdown…`): nothing to fix in the project.
  Check whether the gateway-health deferral is live (entry 1) and whether the binary is current
  (entry 8).
- `stalled: no progress …` rows: the project's session stopped reporting. Look at the project's
  own tick, then at the gateway (`/api/v1/health` → `gateway_errors`).
- A project-side non-zero exit (`exit status N`, an error naming the project's own tool): that is
  the project's problem; hand it to the project's foreman.
- Timeouts: **there is no timeout backoff by design** — a timeout retries at the normal cooldown.
  Do not "escalate" a timeout by editing a cooldown; that is a documented design decision, not a
  bug. If a lane times out repeatedly, check the project's own tick duration against
  `--tick-timeout` (2 h).

---

## 5. Cap vs load gate — both look like "nothing ran"

**Symptom** — `curl /api/v1/status` is healthy, `active_ticks` is 0 or 1, ticks are not spawning,
and nothing anywhere says **why**. A dozen projects look eligible and none of them start.

**Means** — Two different deferral gates produce the identical outward symptom, and the table
that names them is the `ADMIT` log line (one line per candidate project per evaluation pass,
prefix-free so `grep -E '^ADMIT '` matches it):

- **`reason=cap`** — the project's namespace is at `max_concurrent`, or the global
  `--max-concurrent` slot pool is full. Someone else is running; slots free on completion.
- **`reason=load_gate`** — the 1-minute load average is at or above `--load-gate-threshold`
  (default `0` = disabled; the fleet sets it to `12`). This is a **defer, not a drop**: the work
  stays selected, consumes nothing, and is re-picked once load drops. No cooldown damage.

Both are also reported as events that name the decision, so an operator never has to infer it.

**Confirm** *(read-only)*

```sh
LOG="$HOME/.hermes/coding-hermes/scheduler.log"
grep -c 'reason=cap' "$LOG"; grep -c 'reason=load_gate' "$LOG"; grep -m1 'reason=load_gate' "$LOG"; cat /proc/loadavg
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
199416
6292
ADMIT pass_id=1 eligible=89 admitted=0 deferred=89 ns=coding-hermes cap=8 inflight_running=0 inflight_queued=0 project=9router reason=load_gate
6.92 6.87 6.88 6/8410 1245450
```

> **Honest drift note.** The `cap` and `load_gate` counts are cumulative over the log file and
> **grow while you read**: a re-run minutes after the first measured `200373` for `cap` (from
> `199416`) and unchanged `6292` for `load_gate`. Treat them as magnitudes, not constants — the
> `ADMIT` line quoted below is the durable artifact, and `/api/v1/status`'s
> `admission_counters` is the per-process (restart-resetting) partner to these cumulative log
> counts. The `loadavg` line is likewise a live sample, not a value.

The `ADMIT` line carries everything you need to tell the two apart in one read:

```text
ADMIT pass_id=2089 eligible=146 admitted=0 deferred=146 ns=duckbrain-sync cap=1 inflight_running=1 inflight_queued=0 project=warpfs-sync reason=cap
```

- `reason=cap` + `cap=1` + `inflight_running=1` → **the namespace cap bit.** One lane of that
  namespace is running; this one waits.
- `reason=cap` + `cap=0` or with `inflight_running < cap` → **the global `--max-concurrent`
  pool** bit instead. The vocabulary has one word for both; the header fields disambiguate.
- `reason=load_gate` + `inflight_running=0` → **the load gate** bit. Nobody is running; the box
  is busy (here: load 6.92 against the fleet's threshold of 12 — under it, so this line is
  historical).

The gate's own threshold, live, and the per-gate counters:

```sh
curl -s --max-time 25 http://127.0.0.1:9090/api/v1/config | jq -c '{load_gate_threshold, max_concurrent}'
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
{"load_gate_threshold":12,"max_concurrent":10}
```

```sh
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT COUNT(*) FROM events WHERE component='load_gate';"
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT details FROM events WHERE component='load_gate' ORDER BY id DESC LIMIT 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
3792
{"deferred":true,"load_1m":15.36962890625,"namespace":"coding-hermes","project":"hermes-canopy","reason":"load_gate_deferred","threshold":12}
```

That event is self-explaining: which project, which namespace, the measured load, and the
threshold it crossed. The same deferral is visible on the read API:

```sh
curl -s --max-time 25 "http://127.0.0.1:9090/api/v1/events?component=load_gate&limit=1" | jq -c '.events[0].message'
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
"load gate deferred hermes-canopy"
```

**Fix** — 

- **`reason=cap`** is normally correct behavior — the fleet is saturated and the gates are
  holding. If it is *wrong* (a namespace whose cap should be higher, or a stale in-flight row
  holding a slot), first check entry 6; a stale `queued` row occupies the namespace's in-flight
  count and blocks admission while running nothing.
- **`reason=load_gate`** is the gate working as designed. The threshold is config
  (`--load-gate-threshold`, env `SCHEDULER_LOAD_GATE_THRESHOLD`, TOML
  `[scheduler] load_gate_threshold`); `0` disables it. A namespace can opt out by setting its
  `load_gate` column to `off` — check who has opted out with
  `sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" "SELECT id, max_concurrent, load_gate FROM namespaces;"`.
  **Do not** respond by editing the threshold mid-incident unless the decision is deliberate:
  the gate exists so the box does not overload, and turning it off under load is the condition
  it was written for.
- Neither gate is a "nothing to do" signal. If **both** counters are flat while eligible work
  exists, go to entries 9 and 10 — the failure is upstream of admission.

---

## 6. Stale `queued` rows inflate `active_ticks` and block their projects

**Symptom** — `active_ticks` looks like real work, but no processes exist and the queue view
lists ticks for projects that are demonstrably idle. Those projects never spawn again, and the
namespace in-flight count includes them.

**Means** — A `queued` row is created by the evaluation or nudge path and dispatched by the *same
process*. Every path that returns from a spawn without starting the row (restart, gate
deferral, slot-patience drop) leaves it `queued` with nobody owning it — and a stale `queued`
row is not inert: the in-flight dedup refuses to re-spawn that project, the orphan re-nudge scan
skips it, and the namespace admission count includes it. The startup reaper closes rows older
than 2 × `--tick-timeout` (4 h at the fleet's 2 h).

**Confirm** *(read-only)*

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT COUNT(*) AS queued_rows,
        COALESCE(SUM(julianday(COALESCE(spawned_at,created_at)) < julianday(datetime('now','-4 hours'))),0) AS stale_over_4h
 FROM ticks WHERE status='queued';"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
queued_rows  stale_over_4h
-----------           0
          0            0
```

An honest zero on both counts: at authoring time there were no `queued` rows at all. The
*stale* case is only visible if you catch it before the reaper runs, so corroborate against the
reaper's own record, which is durable in the log:

```sh
grep 'QUEUED: reaped' "$HOME/.hermes/coding-hermes/scheduler.log" | tail -1
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
2026/09/19 09:06:02 tick_process.go:672: QUEUED: reaped 5 stale queued tick(s) never dispatched by the previous process (older than 4h0m0s = 2x tick timeout 2h0m0s)
```

That line is the whole story of the class in one line: five rows, never dispatched, older than
2 × tick timeout, closed by the startup reaper. A **recurring** reap on every restart is the
signal to chase — it means some path is enqueuing rows it never starts.

**Fix** — The startup reaper is the fix and it is automatic; a restart with no interruption takes
care of the accumulated rows (`QUEUED: startup reap — no stale queued rows` when there is nothing
to do). Do **not** hand-run `UPDATE ticks SET status=…` on the live DB: the reaper is
single-writer-safe and derives its window from tick timeout, and a manual update during a
running pass can collide. If the reap recurs on every restart, capture the `QUEUED:` lines and
the queue view at the time and file it — the responsible path (a gate deferral that enqueued
before deferring) is a specific defect class, not a general condition. Related: the runbook
warns that a nonzero `queued` **does not** block a drain — only `running` does.

---

## 7. The board never drains — duplicate findings keep it non-empty

**Symptom** — A board stays non-empty forever, so its lane keeps being selected as having work,
yet the same findings keep coming back. A gate run reports
`VIOLATION board-content-dup [X,X]: identical content (fingerprint …)`.

**Means** — Two separate board conditions, and only one of them is "the scheduler is confused":

- **Writer vocabulary.** Four spellings are dispatchable/closed: `pending`, `in_progress`,
  `complete`, `duplicate`. `in_progress` is the live-lane claim and is legitimate. Spellings
  outside that set (`todo`, `open`, `blocked`, `retired`, `done`, `completed`, `closed`, …) are
  *visible to the fleet and scheduled by nobody* — visible work no lane will pick up.
- **Content duplicates.** Two rows with the same content (ignoring id, timestamps and audit
  prose), where at least one is still open, inflate the pending-boost counter and keep the board
  non-empty no matter how much work is done. `duplicate` is the writer-side terminal status for
  exactly this — it closes a row without pretending the work shipped.

**Confirm** *(read-only)*

```sh
python3 ops/check-fleet-invariants.py --board-only --board /home/kara/consensus/.coding-hermes/board/tasks.jsonl
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
VIOLATION board-content-dup [QA-CONSENSUS-1,QA-CONSENSUS-1]: identical content (fingerprint 8b00dc9c15e5e998)
INFO board-vocab /home/kara/consensus/.coding-hermes/board/tasks.jsonl: 315 row(s) scanned, 0 legacy closed spelling(s)
FAIL — 1 violation(s) (board-only mode: fleet checks 1-7 not run — no local DB/TOML)
```

`--board-only` is the CI mode: it needs no live DB and no TOML, so it runs anywhere. Swap
`--board <path>` to point at any project's board. One line per offending **group**, with every
row id listed — a pair becomes a triple after one more copy-paste.

The vocabulary half prints `board-vocab` lines naming the off-vocabulary status:

```sh
python3 ops/check-fleet-invariants.py --board-only --board /home/kara/bunker/.coding-hermes/board/tasks.jsonl | head -4
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
VIOLATION board-vocab DF-BUNKER-1: status=retired
VIOLATION board-vocab GAP-067: status=blocked
VIOLATION board-vocab QA-BUNKER-5: status=retired
VIOLATION board-vocab QA-BUNKER-1: status=retired
```

Own board clean, for contrast:

```sh
python3 ops/check-fleet-invariants.py --board-only
```

EXPECTED OUTPUT *(real, 2026-09-20, from the scheduler worktree)*

```text
INFO board-vocab /home/kara/worktrees/coding-herms-scheduler-SCHED-GAP-153/.coding-hermes/board/tasks.jsonl: 315 row(s) scanned, 0 legacy closed spelling(s)
PASS — 0 violation(s) (board-only mode: fleet checks 1-7 not run — no local DB/TOML)
```

**Fix** — 

- **Content duplicates:** close the extra copies with status `duplicate` (that is what the
  vocabulary reserves it for), keeping the surviving row. Do not delete rows — the gate reports
  groups precisely so the closure is a decision, not a cleanup.
- **Off-vocabulary statuses:** re-spell to `pending`/`in_progress`/`complete`. The legacy closed
  spellings (`done`, `completed`, `closed`) are reported in their own class because they mark
  *finished* work and can be swept to `complete`; parked spellings (`todo`, `open`, …) are
  writer bugs and must be re-filed as `pending` or deliberately closed. `blocked` and `retired`
  are outside the vocabulary on purpose and will keep failing the gate until re-filed.
- Run the gate on the board after any board edit; it is read-only and cheap.

---

## 8. "Daemon predates fix" — the restart loaded a stale binary

**Symptom** — The fix is merged on `main`, the daemon answers `status: ok` with a fresh uptime,
ticks spawn — and the fix is not there. New log lines never appear, new JSON keys are missing
from `/api/v1/status`.

**Means** — The systemd unit rebuilds from source on every start
(`ExecStartPre=…/schedulerd-build.sh`), so **a restart is a deploy**: whatever is committed at
that instant becomes the running binary. A restart that loads a binary built before your fix
looks completely healthy while the fleet runs pre-fix code. `version` on `/api/v1/health` is a
decorative string that cannot be correlated to a commit — which is why the freshness gate exists
and compares commit *topology*, not timestamps.

**Confirm** *(read-only)*

```sh
cd /home/kara/coding-hermes-scheduler/coding-herms-scheduler   # the repo, not the outer dir
ops/check-daemon-freshness.sh; echo "EXIT=$?"
```

EXPECTED OUTPUT *(real, 2026-09-20 — the SCHED-GAP-162 deploy gap, still open)*

```text
check-daemon-freshness: live 1f8e95ee is an ANCESTOR of reference 19e9fd6d — the daemon was built before that change landed (restart from a rebuilt binary)
STALE: live=1f8e95ee reference=19e9fd6d
EXIT=1
```

Exit codes: `0` `FRESH` · `1` `STALE` · `2` `UNKNOWN`. `UNKNOWN` (2) is a failure for deploy
purposes, not a "nothing to see" — it means the daemon cannot state which commit it was built
from at all. `--against <rev>` measures against a specific commit (default `HEAD`);
`--status-url <url>` targets a non-default endpoint.

The same verdict from the API, without the repo:

```sh
curl -s --max-time 25 http://127.0.0.1:9090/api/v1/health | jq -c '{build_sha, build_time}'
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
{"build_sha":"1f8e95e","build_time":"2026-09-19T15:46:11Z"}
```

Compare that sha against the newest commit touching the scheduling surface:

```sh
git -C /home/kara/coding-hermes-scheduler/coding-herms-scheduler log -1 --format='%h %cI %s' -- internal/scheduler internal/database/migrations.go
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
19e9fd6d 2026-09-20T07:51:07-05:00 merge wt/SCHED-GAP-198: deterministic scheduler tests (clock seam instead of ambient timing/goroutine counts)
```

`build_time` `2026-09-19T15:46:11Z` and commit `19e9fd6d` at `2026-09-20T07:51:07-05:00`:
**the merge is newer than the binary.** That comparison, in one line, is what a stale deploy
looks like.

Do not over-read a stale verdict, though. `STALE` means the binary predates *the newest commit
touching the scheduling surface* — it does **not** mean every fix is missing. A concrete,
measured example: entry 1's `failure_reason` stamping landed in `7c8e7499` on 2026-09-18, and
this same stale `1f8e95e` build **does** contain it (6 of 6 gateway failures since daemon start
carry the stamp). Check for the specific behaviour you need — a stamped row, a JSON key, a log
token — before concluding that "the daemon is stale" means "my fix is absent."

**Fix** — Fix the *order*, not the restart: commit or merge the fix first, confirm it is on
`main`, then run the restart procedure. **See `docs/runbook-drain-restart.md` §5 (verify the
restart loaded a current build) and §8 Trap B.** Never re-run a restart hoping the same
uncommitted tree produces a different binary. If the fix *is* committed and the restart still
does not pick it up, the `ExecStartPre` build failed — check the journal for `BUILD FAILED` and
the build log.

---

## 9. Evaluation stalled — the fleet is idle and nothing re-triggers it

**Symptom** — All projects are in cooldown, zero ticks are running, and the fleet simply stops
being evaluated. Nothing alerts. Cooldown-expired projects sit unscheduled for hours.

**Means** — The evaluation loop is event-driven (startup plus a slot-freed debounce). When the
fleet is fully idle nothing re-triggers evaluation, so "no ticks" becomes self-sustaining. A
30-second health ticker watches the age of the last evaluation: past 10 × `--min-interval`
(5 minutes at the fleet's 30 s) with zero running ticks it **forces** a re-evaluation and emits
a `loop` event. On a healthy idle fleet the condition legitimately recurs, so a detection whose
previous forced eval was consumed is demoted to `MEDIUM` (`… (recovered)`) while first onset and
non-recovery stay `HIGH`. Both severities share a 30-minute re-emit throttle.

**Confirm** *(read-only)*

```sh
curl -s --max-time 25 http://127.0.0.1:9090/api/v1/status | jq -c '{zero_select_consecutive, zero_select_eligible, zero_select_last_at}'
```

EXPECTED OUTPUT *(real, 2026-09-20, healthy)*

```text
{"zero_select_consecutive":0,"zero_select_eligible":0,"zero_select_last_at":""}
```

```sh
LOG="$HOME/.hermes/coding-hermes/scheduler.log"
grep -c 'EVAL-STALL' "$LOG"
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT severity, COUNT(*) AS n, MIN(SUBSTR(created_at,1,16)) AS first_seen
 FROM events WHERE message LIKE '%eval loop stalled%' GROUP BY 1 ORDER BY n DESC;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
601
severity   n      first_seen
--------  ---  ----------------
HIGH      364  2026-08-13T20:55
INFO      144  2026-08-27T01:27
MEDIUM     91  2026-08-21T12:57
```

That is **three** severities, and the split is the whole diagnostic: `HIGH` non-recovered is the
wedge, `MEDIUM` is the recovered cadence, and `INFO` is the first non-recovered crossing below
the escalation threshold (transient slowness, not yet a wedge). A `HIGH` count that keeps rising
while `last_evaluation` on `/api/v1/health` stays frozen is the wedge; a steady `MEDIUM` trickle
on a healthy quiet fleet is the watchdog doing its job.

**Fix** — A single stalled detection on an idle fleet is normal and self-corrects — do nothing.
A *persistent* stall (the event re-fires and `evaluation_age_seconds` never resets) means the
forced re-evaluation is not running: check entry 10 (is the loop selecting nothing?) and then
whether the fleet is paused — a paused fleet emits `EVAL: skipped — loop paused`, which is a
parked fleet, not a stall (see the runbook's §8 Trap D). Do not POST `/api/v1/evaluate` in a
loop; the watchdog already does that for you, and a manual trigger on top of a wedge hides the
wedge.

---

## 10. Zero-select — evaluating, but picking nothing

**Symptom** — `EVAL:` lines stop appearing, ticks do not spawn, and the fleet looks idle — but
unlike entry 9 the loop *is* evaluating. Nothing distinguishes "evaluating" from "evaluating
nothing" from the outside.

**Means** — An evaluation that selects zero projects logs nothing by default. The scheduler now
counts consecutive zero-select evaluations **that had eligible projects available** (enabled, not
running, cooldown elapsed); at 2 consecutive it emits a distinct `EVAL-ZERO-SELECT:` line and a
`HIGH` `loop` event, re-emitted at most every 30 minutes. A zero select with *no* eligible
projects is normal fleet-idle and resets the counter; a zero select while every slot is busy is
expected saturation and also resets it.

**Confirm** *(read-only)*

```sh
curl -s --max-time 25 http://127.0.0.1:9090/api/v1/status | jq -c '{zero_select_consecutive, zero_select_eligible, zero_select_last_at}'
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT severity, COUNT(*) AS n, MIN(SUBSTR(created_at,1,16)) AS first_seen
 FROM events WHERE message LIKE '%selected 0 projects%' GROUP BY 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
{"zero_select_consecutive":0,"zero_select_eligible":0,"zero_select_last_at":""}
severity   n      first_seen
--------  ---  ----------------
HIGH      135  2026-08-13T22:36
```

`zero_select_consecutive = 0` and `zero_select_last_at = ""` is healthy: the fleet either picked
work or had no eligible projects. A non-zero `zero_select_consecutive` with a positive
`zero_select_eligible` is the anomaly — the loop has candidates and is choosing none.

The raw line, in the log and greppable:

```sh
grep -m1 'EVAL-ZERO-SELECT' "$HOME/.hermes/coding-hermes/scheduler.log"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
2026/08/13 17:36:04 loop.go:444: EVAL-ZERO-SELECT: 2 consecutive zero-select eval(s) with 7 eligible project(s) — evaluation is picking nothing
```

**Fix** — When the counter is non-zero, the *next* question is always "why was each candidate
deferred" — and that is exactly what the `ADMIT` lines answer, one per candidate (entry 5). Run
`grep -E '^ADMIT ' "$HOME/.hermes/coding-hermes/scheduler.log" | tail -20` and read the reasons.
The usual answers are `cap` (saturation), `budget` (weight or money), `load_gate`, or `cooldown`
carrying `cooldown_remaining_s`. A zero-select with all candidates at `reason=cooldown` where
the countdowns have elapsed is a stale-pin problem, not a selection problem — check the pin in
both stores (skill `fleet-cooldown-policy.py --verify`; see the runbook on cooldown authority).

---

## 11. A slot wait expired — the project was dropped, not deferred

**Symptom** — A project is selected, the log shows it packed, and then it never runs. Nothing
about it appears in the failure counts.

**Means** — Distinct from both gates in entry 5: when a spawn waits longer than the slot-patience
window (default 300 s) for a free slot, it is **dropped** with a `MEDIUM` `slot_pool` event
naming the project, the tick id, and how long it waited. This one is not a defer — the tick does
not run. Pair it with the `INFO` `slot_pool` events in the same component (slot acquisitions).

**Confirm** *(read-only)*

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT severity, COUNT(*) AS n FROM events WHERE component='slot_pool' GROUP BY 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
severity   n
--------  ----
INFO      3108
MEDIUM      15
```

The `MEDIUM` rows are the drops. One, verbatim:

```sh
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT message || ' ' || details FROM events WHERE component='slot_pool' ORDER BY id DESC LIMIT 1;"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
slot wait expired — dropped h3 {"max_slots":10,"patience_seconds":300,"project":"h3","running":10,"tick_id":"h3-2026-09-20-13-20-15","waited_seconds":300.000457537}
```

Read the JSON: `running` equals `max_slots` (10/10), the wait hit `patience_seconds` (300), and
`waited_seconds` confirms the full window. This is a saturated box, not a broken project.

**Fix** — A single drop on a saturated fleet is correct backpressure. A *pattern* of drops for one
project while other lanes cycle normally means that project is being picked consistently at the
worst moment — usually a cooldown pin shorter than the time a slot stays busy. The slot-patience
window is config (`slot_patience` on `/api/v1/config`). Do not raise it blindly: the drop exists
so a picked project does not hold a reservation indefinitely. If a drop turns into a stale
`queued` row, go to entry 6.

---

## 12. Auto-disable parked live lanes on gateway noise

**Symptom** — A working lane disappears. `GET /api/v1/status` shows a project with
`failure_rate: 1` and `auto_disable_armed: true`, and the events table has
`project auto-disabled: <name> — 91.0% failure rate (91/100 ticks)` from component
`auto-disable`. An operator concludes the lane is broken and starts hunting for its bug.

**Means** — Two rules decide whether that verdict is real, and both must be read before acting:

1. **Harness failures must not count.** One shared classifier decides whether a failure is
   harness-attributable (a gateway refusal, a refused exec fallback, a connection refusal, a
   drain abort) before it feeds per-project rates — the same classifier as entry 1. A lane whose
   last 100 failures were *all* gateway drain 503s is a **healthy** lane sitting behind a broken
   dependency. Before the classifier was shared between the status surface and the enforcer, such
   a lane read `failure_rate = 0.91` with `auto_disable_armed = true` while the enforcer's own
   verdict on the same window was ~0.02.
2. **Two surfaces must agree.** `GET /api/v1/status` computes what is *armed*; the enforcer
   (`CheckFailureRateAutoDisable`) decides what is *actually parked*. A lane can be advertised as
   armed and never be disabled, or be disabled by the API for an unrelated reason while still
   reading armed. Never infer a park from the rate alone — read the provenance.

The wave this entry exists for happened on **2026-09-16/17**: eight lanes were auto-disabled
inside ~35 minutes, every one of them on failures that were 91–99% gateway text.

**Confirm** *(read-only)*

```sh
curl -s --max-time 60 http://127.0.0.1:9090/api/v1/status | jq -c '.projects_failure_rates | to_entries | map(select(.value.auto_disable_armed==true)) | {armed: length, names: [.[].key]}'
sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT COUNT(*) AS armed_lanes_that_are_actually_enabled FROM projects WHERE enabled=1 AND name IN ('sim-beta','sim-delta');"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
{"armed":2,"names":["sim-beta","sim-delta"]}
0
```

The second number is the one that answers the question: **zero** of the armed projects are
enabled lanes.

Then **read the names against the enabled set** — this is the step that turns a scary number into
an answer:

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT name, enabled, disabled_by, disabled_reason FROM projects WHERE name IN ('sim-beta','sim-delta');"
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
  name     enabled  disabled_by    disabled_reason
---------  -------  -----------  -------------------
sim-beta         0  legacy       pre-GAP-044 disable
sim-delta        0  legacy       pre-GAP-044 disable
```

Both armed rows are `enabled = 0` fixtures that were disabled long before this feature existed
(`disabled_by = legacy`). **No live lane is armed** — that is the healthy reading. An operator who
greps for `armed` without reading the names sees "2 lanes about to be parked."

The historical wave, from the events table, with the per-lane gateway share beside it:

```sh
sqlite3 -header -column -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
"SELECT SUBSTR(created_at,1,16) AS t, SUBSTR(message,1,80) AS message
 FROM events WHERE component='auto-disable' ORDER BY id DESC;"
```

EXPECTED OUTPUT *(real, 2026-09-20, all eight rows)*

```text
       t                                    message
----------------  ------------------------------------------------------------------------
2026-09-17T01:12  project auto-disabled: hermes-canopy — 91.0% failure rate (91/100 ticks)
2026-09-17T01:12  project auto-disabled: hermes-dagger — 91.0% failure rate (91/100 ticks)
2026-09-17T01:05  project auto-disabled: hermes-canopy — 90.0% failure rate (90/100 ticks)
2026-09-17T00:41  project auto-disabled: hermes-dagger — 90.0% failure rate (90/100 ticks)
2026-09-16T23:46  project auto-disabled: heading — 90.0% failure rate (90/100 ticks)
2026-09-16T23:46  project auto-disabled: chimera-v2 — 90.0% failure rate (90/100 ticks)
2026-09-16T23:46  project auto-disabled: bunker — 90.0% failure rate (90/100 ticks)
2026-09-16T23:38  project auto-disabled: crier — 90.0% failure rate (90/100 ticks)
```

Every one of those lanes was parked inside 35 minutes. Now check whether their failures were
theirs at all:

```sh
for p in crier bunker chimera-v2 heading hermes-canopy hermes-dagger; do
  echo -n "$p  "
  sqlite3 -readonly "$HOME/.hermes/coding-hermes/scheduler.db" \
  "SELECT COUNT(*)||' failed, '||SUM(lower(coalesce(error,'')) LIKE '%gateway%')||' gateway'
   FROM ticks WHERE status IN ('failed','timeout') AND project_name='$p'
     AND created_at >= '2026-09-15' AND created_at < '2026-09-18';"
done
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
crier  98 failed, 98 gateway
bunker  95 failed, 95 gateway
chimera-v2  91 failed, 91 gateway
heading  106 failed, 104 gateway
hermes-canopy  93 failed, 92 gateway
hermes-dagger  99 failed, 98 gateway
```

Read it as a single fact: **every one of those failures was the gateway**, not the lane. Six
healthy projects were parked for a gateway outage. The classifier is what prevents that verdict
from being reached again, and the same lanes read clean once it was deployed:

```sh
curl -s --max-time 90 http://127.0.0.1:9090/api/v1/status | jq -c '.projects_failure_rates | to_entries | map(select(.key|IN("crier","bunker","chimera-v2","heading","hermes-canopy","hermes-dagger"))) | .[]'
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
{"key":"bunker","value":{"failed":0,"total":60,"failure_rate":0,"auto_disable_armed":false}}
{"key":"chimera-v2","value":{"failed":0,"total":44,"failure_rate":0,"auto_disable_armed":false}}
{"key":"crier","value":{"failed":1,"total":75,"failure_rate":0.0133,"auto_disable_armed":false}}
{"key":"heading","value":{"failed":2,"total":18,"failure_rate":0.1111,"auto_disable_armed":false}}
{"key":"hermes-canopy","value":{"failed":0,"total":66,"failure_rate":0,"auto_disable_armed":false}}
{"key":"hermes-dagger","value":{"failed":1,"total":72,"failure_rate":0.0138,"auto_disable_armed":false}}
```

Zero to 11% failure rates, none armed. The live policy, for completeness:

```sh
curl -s --max-time 90 http://127.0.0.1:9090/api/v1/status | jq -c '{failure_window, auto_disable}'
```

EXPECTED OUTPUT *(real, 2026-09-20)*

```text
{"failure_window":100,"auto_disable":{"enabled":true,"min_ticks":50,"threshold":0.9,"window":100}}
```

Note this honestly: on this daemon the feature is **on** (`enabled: true`, threshold `0.9`,
50-tick minimum). The default-off wording in the code is about the *default* `--auto-disable-failure-rate=0`;
the fleet runs with it set. So the "nothing can be parked" reassurance does **not** apply here —
what protects the fleet is rule 1, the classifier.

**Fix** — 

- A rate built from `gateway…` failures is the entry-1 class and the fix belongs there, not in the
  project. Confirm the classifier is live by checking that recent gateway-text failures carry a
  `failure_reason` (entry 1) — if they do not, the running binary predates it (entry 8) and the
  rate is inflated for that reason alone.
- To re-enable a lane that was auto-disabled, use the API
  (`POST /api/v1/projects/{name}/resume`) so the `disabled_*` fields are cleared properly; do not
  hand-edit the row. Note that `disabled_by` tells you *why* it is off — `auto-disable` means the
  rate rule fired, `api` means an operator or script did it, `legacy` means it predates the
  provenance fields entirely. Check that before resuming a lane: a lane someone disabled
  deliberately must not be re-enabled by a rate investigation.
- If auto-disable is firing on genuinely project-side failures, the rate is telling the truth;
  take it to the project's foreman rather than to the scheduler.

---

## Appendix — evidence log

Read-only, executed 2026-09-20 while authoring this page, on the live fleet host (daemon uptime
~21h50m, `build_sha 1f8e95e`, repo HEAD `19e9fd6d`).

| Entry | Command kind | Result |
|-------|--------------|--------|
| surfaces | `curl /api/v1/health` | `status ok`, `build_sha 1f8e95e`, `active_ticks 9`, `gateway_errors 25` |
| 1 | `sqlite3` failed/timeout split, 7 d | `915` failures, `797` were HTTP 503 drain rows; `909` lanes have no `failure_reason` (pre-daemon-start rows) |
| 1 | `sqlite3` gateway fails since daemon start | `6` rows, `6` stamped — the classifier is live in `1f8e95e` |
| 1 | `sqlite3` one 503 row | `failed/failed/0/""/gateway unreachable … HTTP 503: … Gateway is draining existing work` |
| 2 | `grep` RESUME defers | `25` `at cap` defers, example quoted |
| 2 | `sqlite3` resume events + nudge counts | `HIGH 221` resume events, `HIGH 163` of them `needs human`; newest quoted; `nudge_count` max = 2 |
| 3 | `sqlite3` orphan reasons | `drain_timeout 27` (09-03 → 09-18), `zombie_reap 17` |
| 3 | `sqlite3` drain rows + error | 3 rows quoted; `aborted by graceful shutdown — drain timed out with tick in flight` |
| 4 | `sqlite3` status/outcome/exit_code/error matrix, 7 d | 8 distinct shapes quoted |
| 5 | `grep` ADMIT reasons | `reason=cap` 199,416 (grows — re-measured 200,373) · `reason=load_gate` 6,292 · `/proc/loadavg` quoted |
| 5 | `curl /api/v1/config` | `load_gate_threshold 12`, `max_concurrent 10` |
| 5 | `sqlite3` + `curl` load-gate events | 3,792 events; details JSON quoted |
| 6 | `sqlite3` queued rows | `0` queued, `0` older than 4 h |
| 6 | `grep` QUEUED reaps | last reap: 5 rows, never dispatched, 4 h window |
| 7 | `python3 ops/check-fleet-invariants.py --board-only --board <consensus>` | `board-content-dup` + `FAIL — 1 violation(s)` |
| 7 | same, `<bunker>` board and the scheduler worktree board | `board-vocab` violations quoted; worktree board `PASS — 0 violation(s)` |
| 8 | `ops/check-daemon-freshness.sh` | `STALE: live=1f8e95ee reference=19e9fd6d`, `EXIT=1` |
| 8 | `curl /api/v1/health` + `git log -1 -- internal/scheduler …` | `build_time 2026-09-19T15:46:11Z` vs commit `19e9fd6d 2026-09-20T07:51:07-05:00`; stale ≠ every fix missing (the `7c8e7499` stamp is present) |
| 9 | `grep` EVAL-STALL + `sqlite3` stall events | `601` log lines; `HIGH 364`, `INFO 144`, `MEDIUM 91` (three severities — the split is the diagnostic) |
| 10 | `curl /api/v1/status` zero-select + `sqlite3` events | all-zero diagnostics; `HIGH 135` zero-select events, first seen 2026-08-13; log line quoted |
| 11 | `sqlite3` slot_pool events | `INFO 3108`, `MEDIUM 15`; newest drop details quoted |
| 12 | `curl /api/v1/status` armed rates + `sqlite3` provenance | 2 armed, both `enabled=0` `legacy` fixtures — no live lane armed; `auto_disable` block is `enabled:true, threshold:0.9` |
| 12 | `sqlite3` auto-disable events + per-lane gateway share | 8 lanes parked 09-16/17 (`crier` 98/98 gateway, `bunker` 95/95, `chimera-v2` 91/91, `heading` 104/106, `hermes-canopy` 92/93, `hermes-dagger` 98/99); all six now 0–11% and unarmed |

Not executed by the author: nothing in this page. Every `*(read-only)*` command above was run
and its real output is what is quoted. The page contains no `*(not executed)*` command — this is
diagnosis-only, and every entry here is answerable from the API, the DB, or the log.

No credential value appears on this page, and no privileged command was run to produce it. Every
query is a `SELECT`; the API is unauthenticated loopback; the only mutation suggested anywhere is
via the documented REST endpoints or the sanctioned ops scripts.
