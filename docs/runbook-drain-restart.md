# Runbook — drain-restart the fleet scheduler daemon

**Applies to:** `coding-hermes-scheduler` (the `schedulerd` daemon) on the fleet host.
**Audience:** an operator who has never done this. Every step is one copy-pasteable command with the
output it must produce.
**Owner:** `coding-hermes-scheduler` project. **Related:** SCHED-GAP-121 (cooldown authority),
SCHED-GAP-142 (stale-build restart), SCHED-GAP-148 (daemon-freshness guard), SCHED-GAP-155 (ADMIT
log), SCHED-GAP-162 (open deploy gap).

---

## 0. How to read this page

Every command below is tagged:

| Tag | Meaning |
|-----|---------|
| *(read-only)* | Safe to run on production at any time. **Executed while authoring this page; its real output is quoted.** |
| *(mutating)* | Changes the running daemon. Marked `# NOT executed in authoring — expected output shown` with the expected **shape** stated. Nobody executed these to write this page. |

Two facts drive the whole procedure:

1. **The unit rebuilds from source on every start.** `ExecStartPre=/home/kara/.hermes/scripts/schedulerd-build.sh`
   runs `make build` before the daemon starts. A restart **is** a deploy: whatever is on `main` at that
   moment becomes the running binary. If your fix is not committed yet, the restart deploys the old code.
2. **"Restarted" is not "deployed".** A restart that loads a binary built before your fix looks
   completely healthy (`status: ok`, uptime resets, ticks spawn) while the fleet runs pre-fix code.
   This has happened twice (see §8 Trap B). Steps 5.1–5.5 exist to make that outcome detectable.

**Time budget:** 5–40 min, dominated by the drain. Running ticks must finish on their own; the tick
timeout is `7200s` (2 h), so a drain can legitimately take that long. Do not shorten it by killing ticks.
**Busy-fleet exception:** §4b, below — an operator-APPROVED restart may proceed without
`active_ticks=0`, with the in-flight reaps it causes acknowledged in advance and excluded from lane
accounting. The default path (§2–§7) stays conservative.

**Repo (build + ops scripts):** `/home/kara/coding-hermes-scheduler/coding-herms-scheduler/`
(the outer `/home/kara/coding-hermes-scheduler/` directory is **not** the repo).
**Database (read-only queries):** `/home/kara/.hermes/coding-hermes/scheduler.db`.
**Service endpoints:** `http://127.0.0.1:9090` — loopback, but **GETs are open while mutations are
NOT**. `POST /api/v1/pause`, `POST /api/v1/resume`, every `PUT`/`DELETE` and the peer routes pass
through the daemon's operator gate (SCHED-GAP-1602) and accept only the credential the DAEMON loaded
from its own `EnvironmentFile` (`~/.hermes/scheduler-auth.env`, `SCHEDULER_OPERATOR_TOKEN=`). Missing
header → 401 `operator credential required for mutations`; wrong value → 401 `invalid operator
credential`; no credential configured daemon-side → 503. `API_SERVER_KEY` from `~/.hermes/.env` is the
**gateway** key and this gate refuses it — that mix-up is SCHED-GAP-1756 (§8 Trap E).

Sanity-check the repo path before you start:

```sh
cd /home/kara/coding-hermes-scheduler/coding-herms-scheduler && pwd
```

EXPECTED OUTPUT

```text
/home/kara/coding-hermes-scheduler/coding-herms-scheduler
```

---

## 1. Pre-flight — identify the unit and record the baseline (read-only)

Do not skip this section. Every trap in §8 is caught here.

### 1.1 Prove which unit owns the daemon

There are **two** unit files on this host with the same name. Only the **user** unit runs the fleet.

```sh
systemctl --user show coding-hermes-scheduler.service -p FragmentPath -p DropInPaths -p MainPID -p ActiveEnterTimestamp -p NRestarts -p Restart -p RestartUSec -p EnvironmentFiles
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
FragmentPath=/home/kara/.config/systemd/user/coding-hermes-scheduler.service
DropInPaths=/home/kara/.config/systemd/user/coding-hermes-scheduler.service.d/dbgap031-auth.conf /home/kara/.config/systemd/user/coding-hermes-scheduler.service.d/loadgate.conf /home/kara/.config/systemd/user/coding-hermes-scheduler.service.d/task-router.conf
ActiveEnterTimestamp=Thu 2026-09-17 23:51:17 -05
Restart=on-failure
MainPID=923043
NRestarts=0
EnvironmentFiles=/etc/coding-hermes/gateway.env (ignore_errors=yes)
RestartUSec=30s
```

What you must see:

- `FragmentPath` under `/home/kara/.config/systemd/user/` — **not** `/etc/systemd/system/` (§8 Trap C).
- `EnvironmentFiles=/etc/coding-hermes/gateway.env (ignore_errors=yes)` — the gateway key arrives by
  env file. There is **no** `--gateway-key` on the command line, deliberately: `argv` is readable by any
  local user (`ps`), a 0600 env file is not.
- `ActiveEnterTimestamp` — write this down. It is your proof-of-restart in step 5.

Then prove the live PID really is that unit's child (not a stray manual launch):

```sh
cat /proc/$(systemctl --user show coding-hermes-scheduler.service -p MainPID --value)/cgroup
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
0::/user.slice/user-1000.slice/user@1000.service/app.slice/coding-hermes-scheduler.service
```

If the cgroup says anything else, a hand-launched daemon owns the port and no amount of
`systemctl --user restart` will deploy anything. Stop and find out who launched it.

### 1.2 Confirm the dormant twin is still dormant

```sh
systemctl --user is-enabled coding-hermes-scheduler.service; systemctl --user is-active coding-hermes-scheduler.service; systemctl is-enabled coding-hermes-scheduler.service; systemctl is-active coding-hermes-scheduler.service
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
enabled
active
disabled
inactive
```

Read it as two pairs: **`enabled`/`active`** = the user unit (authoritative);
**`disabled`/`inactive`** = `/etc/systemd/system/coding-hermes-scheduler.service` (the relic, §8 Trap C).

### 1.3 Record the live daemon's flags

```sh
ps -o args= -p "$(systemctl --user show coding-hermes-scheduler.service -p MainPID --value)"
```

EXPECTED OUTPUT *(real, 2026-09-18; line wrapped here, one line on the host)*

```text
/home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd -db /home/kara/.hermes/coding-hermes/scheduler.db -config /home/kara/.hermes/fleet.toml -listen 127.0.0.1:9090 --namespace-mode --max-concurrent 10 --min-interval 30s --tick-timeout 7200s --gateway-url=http://127.0.0.1:8642 --no-exec-fallback --auto-disable-failure-rate 0.9 --groups-file /home/kara/.hermes/coding-hermes/groups.jsonl --templates-file /home/kara/.hermes/coding-hermes/templates.jsonl
```

> **Do not use `ps -eo args= -C schedulerd` for this.** Adding `-e` overrides `-C` and dumps every
> process on the host — verified: `ps -eo args= -C schedulerd | wc -l` → `1668` lines, while
> `ps -o args= -C schedulerd | wc -l` → `1`. The `-e` form can match an unrelated process whose argv
> merely quotes the daemon's flags, so `head -1` is a coin flip. Anchor on `MainPID` as above.

### 1.4 Record the build identity and uptime — BEFORE you restart

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/health | jq '{status, version, uptime, active_ticks}'
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```json
{
  "status": "ok",
  "version": "v1.3.0-44-g8afac21-dirty",
  "uptime": "3h54m17.3361711s",
  "active_ticks": 9
}
```

`uptime` is your reset witness: after a real restart it must be seconds, not hours. `version` is a
**decorative** string — it cannot be correlated to a commit, which is why step 1.6 and step 5 exist.

### 1.5 Check the pause flag FIRST — this is Trap D

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq '{paused, active_ticks, budget_total, budget_source}'
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```json
{
  "paused": false,
  "active_ticks": 9,
  "budget_total": 100,
  "budget_source": "flag-default"
}
```

**`paused: true` on entry means a previous drain was abandoned.** Do not treat this as your pause —
you have inherited someone else's half-finished run. Resume first (§8 Trap A), then start this runbook
from §2. Also: `paused` lives in the daemon's memory, so a restart silently clears it — "the fleet
came back up" is not evidence anyone resumed it deliberately.

### 1.6 Take the freshness + invariant baseline (the two ops gates)

```sh
ops/check-daemon-freshness.sh; echo "EXIT=$?"
```

EXPECTED OUTPUT *(real, 2026-09-18 — this is the SCHED-GAP-162 deploy gap, see §8 Trap B)*

```text
check-daemon-freshness: schedulerd@127.0.0.1:9090 reports no usable build_sha (got '<missing>') at http://127.0.0.1:9090/api/v1/status
  a daemon predating the build-identity wiring cannot answer freshness questions — rebuild and restart it
UNKNOWN: live=<missing> reference=b57c80f2 (schedulerd@127.0.0.1:9090; no usable build_sha at http://127.0.0.1:9090/api/v1/status)
EXIT=2
```

Exit codes: `0` FRESH · `1` STALE · `2` UNKNOWN. **`2` UNKNOWN is a failure for deploy purposes**, not
a "nothing to see here": it means the daemon cannot state which commit it was built from. After a
successful restart this must print `FRESH:` with a real sha.

```sh
python3 ops/check-fleet-invariants.py; echo "EXIT=$?"
```

EXPECTED OUTPUT *(real, 2026-09-18; `INFO` lines abridged)*

```text
INFO targets deepseek-payg-sync: external data target 'deepseek-payg' (no repo/project) — lane is active, verified by its own workdir + recent completed tick
INFO targets gitreins-sync: external data target 'gitreins' (no repo/project) — lane is active, verified by its own workdir + recent completed tick
INFO targets h3-umbrella-sync: external data target 'h3-umbrella' (no repo/project) — lane is active, verified by its own workdir + recent completed tick
PASS — 0 violation(s) across 12 namespaces / 89 enabled lanes
EXIT=0
```

Exit `0` = every invariant holds (caps, admission modes, cooldown law, executors, workdirs, satellite
targets, DB↔TOML store parity). Exit `1` prints `VIOLATION <class> <subject>: <detail>` lines.
Run this **before and after** the restart; a violation that appears only after means the restart picked
up different config than the one you had.

### 1.7 Record caps and admission modes

`/api/v1/status` does **not** expose namespace caps or admission modes — do not go looking for a cap
field there (it carries `wave_workers_cap_configured`, which is the wave worker cap, not the namespace
cap). Caps live in the database and on the process argv:

```sh
sqlite3 "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" "SELECT id, max_concurrent, admission_mode FROM namespaces ORDER BY max_concurrent DESC, id;"
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
coding-hermes|8|tasks
doc-writer|1|cooldown
dogfood|1|cooldown
duckbrain-sync|1|cooldown
pm|1|cooldown
qa|1|cooldown
releases|1|cooldown
update-and-patch|1|cooldown
backup|0|cooldown
data-cleanup|0|cooldown
duckbrain-infra|0|cooldown
monitoring|0|cooldown
```

The global cap is the `--max-concurrent` value from step 1.3:

```sh
ps -o args= -p "$(systemctl --user show coding-hermes-scheduler.service -p MainPID --value)" | grep -o -- '--max-concurrent [0-9]*'
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
--max-concurrent 10
```

---

## 2. Pause — stop admitting new work

*(mutating — not run while authoring this page)*

The pause is authenticated: send the daemon's own operator token, out of `argv` (`ps` is readable).

```sh
TOKEN=$(sed -n 's/^SCHEDULER_OPERATOR_TOKEN=//p' "$HOME/.hermes/scheduler-auth.env")
curl -s -o /tmp/pause.json -w '%{http_code}\n' -X POST \
  -K <(printf 'header = "X-Operator-Token: %s"\n' "$TOKEN") \
  http://127.0.0.1:9090/api/v1/pause
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT

```text
200
```

`POST` is required; a `GET` returns `405 {"error":"POST only"}`. Read the CODE, never assume: a `401`
here means the credential, not the endpoint — `{"error":"invalid operator credential"}` is the wrong
token, `{"error":"operator credential required for mutations"}` is a missing header, `503 mutations
disabled` means the daemon has no operator credential configured at all. A rejected pause leaves the
fleet RUNNING while you believe it is parked, which is the failure SCHED-GAP-1756 records.

Verify the pause took effect:

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq .paused
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT

```text
true
```

From this moment the scheduler admits nothing new. **Scheduling is now down and stays down until you
resume — nothing else on this host will resume it for you.** If your shell dies here, go to §8 Trap A.

---

## 3. Drain — wait for in-flight ticks to finish (read-only)

A restart while a spawn is in flight orphans that tick and books a failure the project never caused.
Wait for zero.

First confirm the status vocabulary you are about to query — never assume it:

```sh
sqlite3 "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" "SELECT DISTINCT status FROM ticks;"
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
completed
failed
queued
running
timeout
```

The in-flight statuses are **`queued` and `running`** (the column is constrained:
`CHECK(status IN ('queued','running','completed','failed','timeout','deferred'))` — `deferred` is a
terminal gateway-side deferral, SCHED-GAP-203, never in flight). Gate the drain on the endpoint
first, because that is what the daemon itself uses:

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq '.active_ticks'
```

EXPECTED OUTPUT *(real, 2026-09-18, pre-drain)*

```text
9
```

Cross-check against the database — `running` is the count that must reach zero:

```sh
sqlite3 "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" "SELECT (SELECT COUNT(*) FROM ticks WHERE status='running') AS running, (SELECT COUNT(*) FROM ticks WHERE status='queued') AS queued;"
```

EXPECTED OUTPUT *(real, 2026-09-18, pre-drain)*

```text
9|7
```

**Drained condition: `running = 0`** (equivalently `active_ticks = 0`; in the pre-drain sample both
endpoint and SQL read 9, so they agree).

> **The `queued` number does not reach zero, and that is not a failed drain.** `queued` rows can be
> stale reservations left by an interrupted pass, not live work: in the sample above all 7 `queued`
> rows were spawned on 2026-09-16/17 with `pid = 0` and never completed. Inspect them before you
> believe a nonzero `queued` is blocking you:

```sh
sqlite3 -header -column "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" "SELECT id, project_name, status, spawned_at, pid FROM ticks WHERE status IN ('queued','running') ORDER BY spawned_at DESC LIMIT 20;"
```

EXPECTED OUTPUT **shape**: one row per in-flight tick; `running` rows carry a recent `spawned_at`
(within the last few minutes), stale `queued` rows carry an old one and `pid = 0`:

```text
id                                            project_name     status   spawned_at              pid
--------------------------------------------  ---------------  -------  ----------------------  ---
gitreins-poc-2026-09-18-08-41-20              gitreins-poc    running  2026-09-18T03:41:20-05:00  0
my-project-2026-09-18-08-41-20                my-project      running  2026-09-18T03:41:20-05:00  0
...
gitreins-poc-2026-09-17-01-07-35-nudge1-...   gitreins-poc    queued   2026-09-16T20:17:32-05:00  0
warpfs-2026-09-17-01-07-35-nudge1-...         warpfs          queued   2026-09-16T20:17:32-05:00  0
```

Poll until `running = 0`. Do not kill ticks to speed this up; `--tick-timeout 7200s` is the bound.

---

## 4. Restart the user unit

*(mutating — not run while authoring this page)*

The normal, conservative path reaches `active_ticks=0` first (§3) and then restarts:

```sh
systemctl --user restart coding-hermes-scheduler.service
# NOT executed in authoring — expected output shown
```

### 4b. Busy-fleet, operator-approved restart

A busy fleet may never reach zero: pausing stops admission, but existing ticks can keep
heartbeating until their effective deadline. Do not silently bypass the drain. If deploying
now is more valuable than preserving in-flight work, the operator must explicitly acknowledge
the expected reaps and classify them as transport events:

```sh
systemctl --user set-environment SCHEDULER_OPERATOR_RESTART_APPROVED=1
systemctl --user restart coding-hermes-scheduler.service
systemctl --user unset-environment SCHEDULER_OPERATOR_RESTART_APPROVED
```

The scheduler records reaps from this approved stop as `orphan_reason=operator_restart`.
Legacy `drain_timeout` reaps are also excluded from lane failure-rate and auto-disable accounting,
but the explicit reason is required for this intentional path. Record the expected reap count
before proceeding and verify it afterward; this is an operator decision, not an automatic fallback.
The `set-environment` value must be present in the user manager environment before the restart;
an environment variable prefixed to `systemctl` itself does not reach the daemon.

If the unit is stopped, `kill $MAINPID` does not start it again on this host. Use the authoritative
user-unit verb:

```sh
systemctl --user start coding-hermes-scheduler.service
```

This page documents the contract; the busy-fleet path was rehearsed only against a hermetic
fixture, not by pausing or restarting the live fleet.

EXPECTED OUTPUT **shape** — `systemctl` prints **nothing** on success (a failure prints
`Job for coding-hermes-scheduler.service failed` plus a reason to stderr and exits non-zero). This
command runs `ExecStartPre` first: `make build` rebuilds `bin/schedulerd` from source, and **only then**
does the daemon start. A build failure aborts the start with `BUILD FAILED` in the unit's journal and
leaves the previous process gone — the fleet is down until you fix the build. Check the repo is
buildable first if you are deploying uncommitted work.

Then wait for readiness (up to ~2 min) and confirm the restart actually happened:

```sh
sleep 5; curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9090/api/v1/health
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT

```text
200
```

`ActiveEnterTimestamp` must now be later than the value recorded in step 1.1:

```sh
systemctl --user show coding-hermes-scheduler.service -p ActiveEnterTimestamp -p ExecStartPre
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT **shape** — the timestamp moved forward, and `ExecStartPre` shows the rebuild ran at
that same second and exited 0 (real values from the 2026-09-17 23:51 restart):

```text
ActiveEnterTimestamp=Thu 2026-09-17 23:51:17 -05
ExecStartPre={ path=/home/kara/.hermes/scripts/schedulerd-build.sh ; argv[]=/home/kara/.hermes/scripts/schedulerd-build.sh ; ignore_errors=no ; start_time=[Thu 2026-09-17 23:51:16 -05] ; stop_time=[Thu 2026-09-17 23:51:17 -05] ; pid=920821 ; code=exited ; status=0 }
```

`NRestarts` is the *automatic* restart counter for the current invocation; a manual
`systemctl --user restart` starts a fresh invocation, so `NRestarts=0` afterwards is normal and is not
evidence that nothing happened. `ActiveEnterTimestamp` is the reliable signal.

---

## 5. Verify the restart loaded a CURRENT build — the part that matters

Uptime and `status: ok` prove a process is answering. They do **not** prove it contains your fix. Run
all five checks; each answers a different question.

### 5.1 Uptime reset (did a restart happen?)

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/health | jq '{status, version, uptime}'
```

EXPECTED OUTPUT **shape** — `status: "ok"` and `uptime` in **seconds**, not hours: compare against the
`3h54m17s` you recorded in step 1.4.

### 5.2 The build freshness gate (is the live build from this branch?)

```sh
ops/check-daemon-freshness.sh; echo "EXIT=$?"
```

EXPECTED OUTPUT **shape** — `FRESH:` on stdout, exit `0`, e.g.
`FRESH: live=<sha8> reference=<sha8>`. Anything else is a failed deploy:
`STALE:` (exit 1) means the live build predates the newest admission/scheduling commit on the branch;
`UNKNOWN:` (exit 2, §1.6) means the daemon cannot report a `build_sha` at all.

Useful variants: `ops/check-daemon-freshness.sh --against <rev>` to measure against a specific commit
(default `HEAD`), `--status-url <url>` for a non-default endpoint. The reference is the newest commit
reachable from `--against` that touches `internal/scheduler/` or `internal/database/migrations.go` —
it is an *ancestor* test, not a timestamp comparison, because a daemon can be restarted from an
older binary and look new.

### 5.3 Prove the fix's own signature is in the running binary

A fix that adds a log token, a JSON key, or a metric name leaves a **string literal** in the binary.
Grep for it — this is the check that distinguishes "restarted" from "deployed":

```sh
strings "$(ps -o args= -p "$(systemctl --user show coding-hermes-scheduler.service -p MainPID --value)" | awk '{print $1}')" | grep -c admission_counters
```

EXPECTED OUTPUT **shape** — `0` means the fix is **absent**; non-zero means present. This is the real
state on 2026-09-18 (the literal was introduced by commit `b57c80f`, SCHED-GAP-155):

```text
0
```

Corroborate against the endpoint the fix was supposed to extend:

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq 'has("admission_counters")'
```

EXPECTED OUTPUT **shape** — `true` after the deploy; the real 2026-09-18 value is

```text
false
```

The binary's own build time is the third corroboration:

```sh
stat -c '%y  %n' /home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd
```

EXPECTED OUTPUT **shape** — the mtime must be inside the restart window from step 4
(real value: `2026-09-17 23:51:17.488567059 -0500`, i.e. the same second as `ActiveEnterTimestamp`).

### 5.4 Fleet invariants still hold

```sh
python3 ops/check-fleet-invariants.py; echo "EXIT=$?"
```

EXPECTED OUTPUT: the same `PASS — 0 violation(s) across <N> namespaces / <M> enabled lanes` line and
`EXIT=0` as in step 1.6. A new `VIOLATION` line means the restart changed effective config.

### 5.5 Caps and admission modes survived

Re-run step 1.7 (the `namespaces` query and the `--max-concurrent` probe) and compare: the same
namespace caps and admission modes must come back. A restart that silently changes a cap is worse than
a restart that fails outright, because nothing alerts on it.

---

## 6. Resume

*(mutating — not run while authoring this page)*

Only run this **after** §5 passes. If §5 failed, see §8 Trap B before resuming.

```sh
curl -s -o /tmp/resume.json -w '%{http_code}\n' -X POST \
  -K <(printf 'header = "X-Operator-Token: %s"\n' "$TOKEN") \
  http://127.0.0.1:9090/api/v1/resume
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT

```text
200
```

**A non-200 resume is a FAILURE, not a retry-able detail.** A `401 {"error":"invalid operator
credential"}` means the fleet is still parked and the credential you are holding is not the one the
daemon wants — fix the credential (§8 Trap E) and resume again. Never record "resumed" from a call
whose status code you did not read.

Verify:

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq .paused
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT

```text
false
```

---

## 7. Prove scheduling is live again

A resumed daemon is not a *working* daemon: admission can still be starved by cooldowns, a full slot
pool, failure backoff, or a wedged evaluation loop. Watch for real work.

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq '{paused, active_ticks, last_evaluation, zero_select_consecutive}'
```

EXPECTED OUTPUT **shape** — `paused: false`, `active_ticks` climbing toward the cap as slots free, and
`last_evaluation` advancing. Real pre-drain sample for orientation:

```json
{
  "paused": false,
  "active_ticks": 9,
  "last_evaluation": "2026-09-18T08:43:20Z",
  "zero_select_consecutive": 4
}
```

Then look for an actual spawn or evaluation in the log (read-only):

```sh
journalctl --user -u coding-hermes-scheduler.service -n 30 --no-pager -o short-iso | grep -E 'EVAL:|SPAWN:|SLOT:'
```

EXPECTED OUTPUT **shape** — at least one `EVAL:` line and, within a few minutes, `SLOT: acquired` +
`SPAWN:` lines. Real lines from a healthy running daemon:

```text
2026-09-18T03:49:15-05:00 ... tick_process.go:141: EVAL: 1 project(s) selected, 2/100 budget used
2026-09-18T03:49:15-05:00 ... slot_pool.go:360: SLOT: acquired for chimera-v2 (9/10 running)
2026-09-18T03:49:15-05:00 ... spawn.go:1026: SPAWN: chimera-v2 tick=chimera-v2-2026-09-18-08-49-15 chain=work ...
```

And confirm new ticks are landing in the database:

```sh
sqlite3 "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" "SELECT COUNT(*) FROM ticks WHERE spawned_at >= datetime('now','-15 minutes');"
```

EXPECTED OUTPUT **shape** — a non-zero count within a few minutes of resume (`0` immediately after
resume is normal: nothing may be off cooldown yet). If it stays `0` for longer than the longest
cooldown, check `zero_select_consecutive` and the `EVAL-ZERO-SELECT:` / eval-stall `loop` events —
that is a scheduling problem, not a deploy problem.

**Dashboard cross-check (optional, read-only):** `http://127.0.0.1:9090/` renders the same fleet state.

---

## 8. Recovery — the four traps

### Trap A — the drain helper dies mid-way and leaves the whole fleet paused

**What it looks like:** you pause, then your shell/SSH/session dies (or the helper is killed) before
the restart. The daemon is still up and still paused; nothing schedules; nothing notifies you.

**Precedent (2026-09-17):** a drain-restart helper was killed mid-drain. Its log
(`/tmp/scheduler-cap24-restart.log`) stops abruptly at `2026-09-17T23:36:24` on
`active_ticks=2` with **no** resume line, and the helper's EXIT trap never ran — the old helper trapped
only `EXIT`, which bash does **not** run when it is killed by a signal. The fleet stayed parked until
the daemon was restarted at `23:51:17` (a restart clears the in-memory pause). Cost: **~15 minutes of
zero scheduling**, visible as a hole in the tick record (last tick `22:52`, next `23:51`) and as
repeated `EVAL: skipped — loop paused` lines in the journal.

**Detection** — the daemon says it out loud:

```sh
journalctl --user -u coding-hermes-scheduler.service -n 200 --no-pager | grep -c 'EVAL: skipped — loop paused'
```

EXPECTED OUTPUT **shape** — `0` on a working fleet. A non-zero, growing count while `paused` is `true`
with no operator in the middle of a drain is the signature of an abandoned drain. Real value on
2026-09-18 (fleet working, daemon up 4 h):

```text
0
```

The journal only holds the current boot; the historical paused windows are still visible in the
scheduler's own log, which is the durable record:

```sh
grep -c 'EVAL: skipped — loop paused' /home/kara/.hermes/coding-hermes/scheduler.log
```

EXPECTED OUTPUT **shape** — cumulative count over the whole log; `68` as of 2026-09-18, most recent
entries inside the 2026-09-18 01:09 paused window.

The authoritative check is the flag itself:

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq .paused
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
false
```

Also check whether a helper is still alive before assuming it died:

```sh
pgrep -af 'drain-restart\.sh' || echo "no drain helper running"
```

EXPECTED OUTPUT *(real, 2026-09-18 — no helper running, and none should be)*

```text
no drain helper running
```

**Action** — if no helper is running and you are not intentionally paused, resume:

```sh
curl -s -X POST http://127.0.0.1:9090/api/v1/resume
# NOT executed in authoring — expected output shown
```

EXPECTED OUTPUT `{"status":"resumed"}` (200), then `paused: false` from the endpoint. Then **verify
work resumes** with §7 before walking away.

**The current helper traps signals** and resumes on `EXIT`, `TERM` and `INT`
(`~/.hermes/scripts/scheduler-deploy-drain-restart.sh`, §Appendix A) — the fix is proven: on
2026-09-18T01:15:25 a killed run logged
`-- EXIT trap (rc=143): died before restart — resuming so the fleet is not left paused` followed by
`resumed at 2026-09-18T01:15:25-05:00`. That protection is not total: a `SIGKILL`, an OOM kill, or a
host reboot still leaves the fleet paused with no notification. **Always check `paused` yourself,
before and after.**

### Trap B — the restart loads a build that PREDATES the fix it was supposed to pick up

**What it looks like:** the restart succeeds, `status: ok`, uptime resets, ticks spawn — and the fix is
not there. Because `ExecStartPre` rebuilds from *source*, a restart deploys whatever is committed at
that instant. If the fix was committed a minute later, you deployed the old code.

**Precedent 1 (2026-09-17, SCHED-GAP-142):** the daemon was restarted at `23:51` (binary built
`23:51:17`) while the orphan-nudge admission fix `1ed5d31` landed at `23:59` — the fleet ran a
pre-fix build and the startup cap breach recurred.

**Precedent 2 (2026-09-18, SCHED-GAP-162, OPEN):** commit `b57c80f` (SCHED-GAP-155 — the structured
`ADMIT` log line + `admission_counters` on `/api/v1/status`) landed at `2026-09-18T02:37:47-05:00`,
while the live daemon is still the `23:51:17` build. Three independent probes agree it is not deployed:
`ops/check-daemon-freshness.sh` → `UNKNOWN: live=<missing> reference=b57c80f2` (exit 2);
`strings bin/schedulerd | grep -c admission_counters` → `0`; `/api/v1/status` has no
`admission_counters` key.

**Detection** — the three probes from §5:

```sh
ops/check-daemon-freshness.sh; echo "EXIT=$?"
```

EXPECTED OUTPUT **shape** — `FRESH:` / exit `0`. `STALE:` (1) or `UNKNOWN:` (2) = not deployed.

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq 'has("admission_counters")'
```

EXPECTED OUTPUT **shape** — `true` once the SCHED-GAP-155 build is live. Real value on 2026-09-18:
`false`.

```sh
ls -l --time-style=full-iso /home/kara/coding-hermes-scheduler/coding-herms-scheduler/bin/schedulerd
```

EXPECTED OUTPUT **shape** — the mtime must be *after* the commit that carries your fix. Compare:

```sh
git -C /home/kara/coding-hermes-scheduler/coding-herms-scheduler log -1 --format='%h %cI %s' -- internal/scheduler internal/database/migrations.go
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
b57c80f 2026-09-18T02:37:47-05:00 feat(scheduler): SCHED-GAP-155 — structured ADMIT admission-decision log + counters
```

That commit time (`02:37:47`) is **after** the binary mtime (`2026-09-17 23:51:17`) — that is exactly
what a stale deploy looks like, in one comparison.

**Action** — fix the *order*, not the restart: commit (or merge) the fix first, confirm the commit is
on `main`, then re-run §2–§7. Never re-run a restart hoping the same uncommitted tree will produce a
different binary; check the freshness gate reads `FRESH` before you call the deploy done. If the fix is
already committed and the restart still does not pick it up, the `ExecStartPre` build failed — check
the journal for `BUILD FAILED` and `/tmp/schedulerd_build.log`.

### Trap C — two units exist: `systemctl --user` is authoritative, `/etc/systemd/system` is a dormant relic (and leaks a live key)

**What it is.** Two unit files carry the name `coding-hermes-scheduler`:

| Path | Owner | State | What it does |
|------|-------|-------|--------------|
| `~/.config/systemd/user/coding-hermes-scheduler.service` | `kara:kara`, mode `600` | `enabled`, `active` | **The real unit.** Runs the daemon; has `-config`, `--groups-file`, `--templates-file`, `ExecStartPre` rebuild, `EnvironmentFile`. |
| `/etc/systemd/system/coding-hermes-scheduler.service` | `root:root`, mode `644` | `disabled`, `inactive` | **Dormant relic.** No `-config`, no `--groups-file`/`--templates-file`. Editing it changes nothing. |

A system service is *not* silently preferred over a user service — but a system unit with the same name
is invisible to `systemctl --user` and vice versa, so an operator who edits the wrong file sees a
successful `daemon-reload` and zero behavior change.

**Detection** — three facts, none of which prints a secret:

```sh
stat -c '%a %U:%G %s %n' /home/kara/.config/systemd/user/coding-hermes-scheduler.service /etc/systemd/system/coding-hermes-scheduler.service
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
600 kara:kara 1905 /home/kara/.config/systemd/user/coding-hermes-scheduler.service
644 root:root 775 /etc/systemd/system/coding-hermes-scheduler.service
```

```sh
systemctl --user cat coding-hermes-scheduler.service | head -1; systemctl cat coding-hermes-scheduler.service | head -1
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
# /home/kara/.config/systemd/user/coding-hermes-scheduler.service
# /etc/systemd/system/coding-hermes-scheduler.service
```

The `--user` form resolves the unit that actually runs; the plain form resolves the relic in
`/etc/systemd/system/`. If you are ever unsure which manager your command is talking to, `FragmentPath`
from §1.1 is the answer.

```sh
grep -c '^Environment=API_SERVER_KEY=' /etc/systemd/system/coding-hermes-scheduler.service; grep -c '^Environment=API_SERVER_KEY=' /home/kara/.config/systemd/user/coding-hermes-scheduler.service; grep -c '^EnvironmentFile=' /home/kara/.config/systemd/user/coding-hermes-scheduler.service
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
1
0
1
```

(Relic: one literal key line. Authoritative unit: zero literal key lines, one `EnvironmentFile`.)
Counts only — **never `cat` that file, and never paste its `Environment=` line anywhere.**

**The leak.** The relic is mode `644` (world-readable) and carries a literal
`Environment=API_SERVER_KEY=<redacted>` holding the **same key** the live daemon uses. Proven without
printing it — compare hashes of just the values, never the values themselves:

```sh
A=$(grep -m1 '^Environment=API_SERVER_KEY=' /etc/systemd/system/coding-hermes-scheduler.service | sed 's/^Environment=API_SERVER_KEY=//'); B=$(grep -m1 '^API_SERVER_KEY=' /etc/coding-hermes/gateway.env | sed 's/^API_SERVER_KEY=//'); [ "$(printf '%s' "$A" | sha256sum | cut -d' ' -f1)" = "$(printf '%s' "$B" | sha256sum | cut -d' ' -f1)" ] && echo "IDENTICAL (sha256 match)" || echo "DIFFERENT"
```

EXPECTED OUTPUT *(real, 2026-09-18)*

```text
IDENTICAL (sha256 match)
```

So a disabled, world-readable file on disk contains the live gateway credential. Removing only the
`Environment=` line (leaving the relic in place) closes the leak; removing the file entirely is
cleaner.

**Ruling — the sanctioned key path.** The **0600 `EnvironmentFile`** (`/etc/coding-hermes/gateway.env`,
mode `600 kara:kara`) is the **only** sanctioned way the gateway key reaches the daemon. The daemon's
`--gateway-key` flag defaults to `$API_SERVER_KEY`, so no key is ever needed on `argv`. Never add
`--gateway-key` to a unit's `ExecStart`: `argv` is readable by every local user via `ps`.

**Remediation.** Removing or scrubbing the relic unit file requires host root privileges and is a
**change-managed operator action**: raise it with the host owner, who should delete the file or strip
its `Environment=` line, then run `systemctl daemon-reload`, and rotate the credential if the file was
readable by anyone it should not have been. **This runbook deliberately publishes no privileged
command for it** — an operator reading this page cold must not remove system unit files unaided. The
detection above is read-only and can be repeated by anyone at any time.

### Trap D — the fleet is paused after a partial drain and nobody noticed

**What it looks like:** a drain-restart was started, then interrupted (Trap A). Hours later the daemon
is healthy, `status: ok`, ticks are simply not spawning. Nothing reports "paused" as an error.

**Detection — ask explicitly, never infer.** `paused` is not a health failure; `/api/v1/health` returns
`status: ok` while the fleet is parked:

```sh
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq .paused
```

EXPECTED OUTPUT **shape** — `false` when the fleet is working. `true` = parked.

The second independent witness is the journal: `EVAL: skipped — loop paused` is emitted whenever an
evaluation is attempted while paused.

```sh
journalctl --user -u coding-hermes-scheduler.service -n 200 --no-pager | grep -c 'EVAL: skipped — loop paused'
```

EXPECTED OUTPUT **shape** — `0` on a working fleet; a non-zero, growing count is a parked fleet. Real
value on 2026-09-18 (fleet working):

```text
0
```

And the third is in the database: a long gap in `spawned_at` with no failures to explain it.

```sh
sqlite3 -header -column "file:$HOME/.hermes/coding-hermes/scheduler.db?mode=ro" "SELECT substr(spawned_at,1,16) AS minute, COUNT(*) AS ticks FROM ticks WHERE spawned_at >= datetime('now','-6 hours') GROUP BY minute ORDER BY minute;"
```

EXPECTED OUTPUT **shape** — a regular cadence of minutes with ticks. A multi-minute-to-hour hole with
no `failed`/`timeout` rows in it is a paused window. Real illustration from the Trap A window:

```text
2026-09-17T22:52         3
2026-09-17T23:51        10
```

— a 59-minute hole with nothing in it: the fleet was paused 22:52→23:36 and then restart-cleared at
23:51.

**Action** — resume (§8 Trap A), then confirm work actually resumes (§7). **Check `paused` at the
start of every drain-restart (§1.5) and again at the end (§6, §7).** Do not assume a daemon that
answers is a daemon that schedules.

### Trap E — the helper holds a credential the daemon does not accept

**What it looks like:** the drain helper logs `pause -> HTTP 401 {"error":"invalid operator
credential"}`, keeps "draining" against a fleet that is still admitting work — or, worse, against a
pause that a DIFFERENT caller set — and its abort path's `resume` is refused with the same credential,
leaving the fleet parked with no owner.

**Precedent (2026-10-22, SCHED-GAP-1756):** `deploy-scheduler-drain-restart.sh` sent `API_SERVER_KEY`
(the gateway key from `~/.hermes/.env`) as `Authorization: Bearer` on every call. The operator gate
refuses that value, so the pause never landed; another caller paused at 07:02; the drain ran 45 min
against that foreign pause and timed out; the resume 401'd at 07:45 and the status code was discarded.
74 minutes of zero admission (07:00–08:14) until a manual restart — which is also what landed the build
the aborted deploy never deployed.

**Detection — status codes and digests, never a credential value:**

```sh
grep -n 'NO WORKING CREDENTIAL\|pause attempt\|RESUME FAILED' /tmp/scheduler-deploy-restart.log | tail
curl -s --max-time 5 http://127.0.0.1:9090/api/v1/status | jq .paused
```

Read the ladder: `pause attempt 1/2 ... -> HTTP 200` means the operator token was accepted; two
`-> HTTP 401` lines mean the daemon recognises NEITHER source. What the daemon actually holds is in its
own environment — compare digests:

```sh
pid=$(pgrep -f 'bin/schedulerd' | head -1)
sed -n 's/^SCHEDULER_OPERATOR_TOKEN=//p' ~/.hermes/scheduler-auth.env | tr -d '\n' | sha256sum
tr '\0' '\n' < /proc/$pid/environ | sed -n 's/^SCHEDULER_OPERATOR_TOKEN=//p' | tr -d '\n' | sha256sum
```

Equal digests = the file is right and the helper's parsing is the problem. Different digests = the
daemon is running a token the file no longer carries (it reads its `EnvironmentFile` at start): fix the
file, then restart to load it.

**Action** — never hand-drive a deploy from a helper you have not proven: pause and resume once by hand
(§2, §6) and read both codes. Then run the helper's own tests
(`~/.hermes/scripts/tests/test-drain-restart-credentials.sh`) before trusting it in a deploy window.

---

## Appendix A — the two drain-restart helpers

Both implement this runbook and both share ONE credential resolver
(`~/.hermes/scripts/lib/scheduler-cred.sh`, SCHED-GAP-1756):

| Script | Shape | Log |
|---|---|---|
| `~/.hermes/scripts/deploy-scheduler-drain-restart.sh` | pause → drain (`DRAIN_MAX`, default 2700 s) → restart → health → resume → symbol + freshness verify | stdout; name it with `DEPLOY_LOG` |
| `~/.hermes/scripts/scheduler-deploy-drain-restart.sh` | same phases plus a namespace/migration dump, the policy-script deploy-hash gate and a board-derived fix-symbol check; drain cap `SCHEDULER_MAX_DRAIN`, default 5 h | `/tmp/scheduler-deploy-restart.log` |

What both guarantee:

- **One credential, resolved before scheduling state is touched.** Pause itself is the probe — rung 1
  `X-Operator-Token`, rung 2 `Authorization: Bearer` (`API_SERVER_KEY`). A 401 changes nothing
  server-side, so a refused rung is a safe probe; if neither is accepted the script prints
  `NO WORKING CREDENTIAL` with both codes and exits `1` having paused nothing. Credentials reach curl
  through a 0600 config file (`-K`), never `argv`.
- **A resume is only "done" at HTTP 200.** 401/403/000 is reported as `RESUME FAILED` (exit `5`) and
  the `paused` flag is read back and must be `false`.
- **It refuses to restart with ticks in flight** — a drain timeout resumes and exits `3` (or `2`).
- **It resumes on death.** `EXIT` *and* `TERM`/`INT` are trapped, because trapping `EXIT` alone did not
  survive a kill (Trap A). A killed run exits `143` and logs the trap's resume.

Two limits you still cover yourself, and they are why this page exists:

1. **`SIGKILL` / OOM / reboot defeat the traps.** Check `paused` by hand (§8 Trap D).
2. **Verification is fix-specific.** The symbol list means nothing for a different change set. Finish
   with §5 — the freshness gate plus your own fix's signature string.

`~/.hermes/scripts/tests/test-drain-restart-credentials.sh` exercises the ladder against a fake daemon
(rung 1, rung 2 fallback, both refused, daemon unreachable, resume-401) and touches no live state. Run
it after editing either helper.

Both helper scripts on this host write their log to `/tmp`; `/tmp` is not durable across a reboot, so
a reboot during a drain leaves *no* record of the abandoned run. That is another reason to check
`paused` explicitly rather than inferring state from logs.

---

## Appendix B — evidence log (what was executed to write this page)

Read-only, executed 2026-09-18 on the live host while the daemon was serving (pid 923043, uptime ~3h54m):

| Command | Result |
|---------|--------|
| repo `pwd` check | correct repo path |
| `systemctl --user show ... -p FragmentPath/DropInPaths/MainPID/ActiveEnterTimestamp/NRestarts/Restart/RestartUSec/EnvironmentFiles` | quoted in §1.1 |
| `systemctl --user is-enabled/is-active` + system-level equivalents | `enabled/active` and `disabled/inactive` (§1.2) |
| `cat /proc/<MainPID>/cgroup` | user-unit cgroup (§1.1) |
| `ps -o args= -p <MainPID>` | full argv quoted in §1.3 |
| `ps -o args= -C schedulerd \| wc -l` vs `ps -eo args= -C schedulerd \| wc -l` | `1` vs `1668` — the `-e` override hazard (§1.3, Appendix A) |
| `curl /api/v1/health` | `status ok`, `version v1.3.0-44-g8afac21-dirty`, `uptime 3h54m17s` (§1.4) |
| `curl /api/v1/status` (fields) | `paused false`, `active_ticks 9`, `budget_total 100`, `budget_source flag-default`, no `admission_counters` key (§1.5, §5.3) |
| `sqlite3 ... "SELECT DISTINCT status FROM ticks;"` | `completed failed queued running timeout` (§3) |
| `sqlite3 ... running/queued counts` | `9\|7` (§3) |
| `sqlite3 ... in-flight rows with spawned_at, pid` | 9 `running` rows spawned minutes earlier; 7 stale `queued` rows from 09-16/17 with `pid=0` (§3) |
| `ops/check-daemon-freshness.sh` | `UNKNOWN: live=<missing> reference=b57c80f2`, exit `2` (§1.6, §8 Trap B) |
| `ops/check-fleet-invariants.py` | `PASS — 0 violation(s) across 12 namespaces / 89 enabled lanes`, exit `0` (§1.6) |
| `sqlite3 ... namespaces` | 12 rows, caps/modes quoted in §1.7 |
| `stat` on both unit files + `grep -c` key-line counts + sha256-only comparison | modes `600`/`644`, counts `1`/`0`/`1`, `IDENTICAL (sha256 match)` (§8 Trap C) |
| `strings bin/schedulerd \| grep -c admission_counters` | `0` — b57c80f not deployed (§5.3) |
| `stat bin/schedulerd` + `git log -1 -- internal/scheduler …` | binary `2026-09-17 23:51:17` vs commit `b57c80f 2026-09-18T02:37:47` (§8 Trap B) |
| `journalctl --user -u coding-hermes-scheduler -n 30` | live `EVAL:`/`SLOT:`/`SPAWN:` lines (§7) |
| `journalctl --user -u ... -n 200 \| grep -c 'EVAL: skipped — loop paused'` | `0` (working fleet) (§8 Trap A/D) |
| `pgrep -af 'drain-restart\.sh'` | `no drain helper running` (§8 Trap A) |
| `sqlite3 ... COUNT(*) ticks in last 15 min` | `112` (§7) |
| `sqlite3 ... spawn-minute histogram, last 6 h` | steady cadence, e.g. `2026-09-18T03:41 → 22` (§8 Trap D) |
| `systemctl --user cat … \| head -1; systemctl cat … \| head -1` | user path vs `/etc/systemd/system/…` (§8 Trap C) |
| `ls -l --time-style=full-iso bin/schedulerd` | `15593737 2026-09-17 23:51:17.488567059` (§5.3, §8 Trap B) |
| read of `/tmp/scheduler-cap24-restart.log` and `/tmp/scheduler-deploy-restart.log` | Trap A precedent: log ends `2026-09-17T23:36:24` on `active_ticks=2`, no resume; and the later trap fix firing at `2026-09-18T01:15:25` (§8 Trap A) |
| `grep -c 'EVAL: skipped — loop paused'` over the scheduler log | `68` lines (§8 Trap A/D) |
| `sqlite3 ... tick minutes 2026-09-17T22:30–2026-09-18T00:20` | `22:52 → 3`, `23:51 → 10` — the paused-window hole (§8 Trap D) |

Not executed by the author (marked `# NOT executed in authoring` in place): `POST /api/v1/pause`,
`POST /api/v1/resume`, and `systemctl --user restart coding-hermes-scheduler.service`. Expected output
shapes for those are derived from the handlers (`internal/api/server.go`) and from recorded real
invocations in the helper logs.

No credential value appears on this page, and no privileged command was run to produce it.
