# SCHED-GAP-1643 — scheduler-watchdog.sh three-case decision evidence

Ops-script proof runs (2026-09-27 ~00:05-00:20 -05). Probe targets and the
restart command were overridden via the script's env knobs; the real
scheduler (schedulerd pid 839395 on 127.0.0.1:9090) was checked before and
after every run and was NEVER restarted. `bash -n scripts/scheduler-watchdog.sh`
→ clean.

## Run i — scheduler live + gateway live → exit 0, silent, no restart

    $ bash scripts/scheduler-watchdog.sh
    exit=0

(No output — the healthy watchdog stays quiet. schedulerd pid unchanged.)

## Run ii — scheduler live (200) + gateway probe pointed at a dead port →
## gateway-dead branch, NO restart, exit non-zero

    $ GATEWAY_HEALTH_URL=http://127.0.0.1:59999/health \
      GATEWAY_PROBE_INTERVAL=2 \
      SCHED_RESTART_CMD="echo STUB-RESTART-COMMAND-INVOKED" \
      bash scripts/scheduler-watchdog.sh
    ⚠️ GATEWAY DOWN, SCHEDULER ALIVE — NOT restarting the scheduler (SCHED-GAP-1643). GET http://127.0.0.1:59999/health -> HTTP 000 after 3 probe(s) x 2s. The scheduler is provably live (http://127.0.0.1:9090/api/v1/live -> 200); its handlers may be slow while its spawns retry against the dead gateway. Restarting the scheduler here is what turned the 2026-09-26 gateway outage into 163 failed ticks. Attend to the gateway, not the scheduler.
    exit=2

    $ grep -c "STUB-RESTART-COMMAND-INVOKED" run-ii.out   # restart stub invocations
    0

LIVE-FIRE CONFIRMATION: the first execution of this exact branch (before the
demo, with all-default URLs) ran during a REAL gateway blip —
http://127.0.0.1:8642/health answered 200 at 23:52, then refused connections
(HTTP 000) for ~10 minutes while /api/v1/live stayed 200. The script printed
the gateway-dead verdict and exited 2; nothing was restarted; the gateway
recovered on its own (fresh hermes pid on 8642) and the run was repeated for
the captured demo above. This is precisely the 09-26 scenario in which the
old script restarted the healthy scheduler (NRestarts=0 + Restart=on-failure
= the restart was explicit).

## Run iii — scheduler live probe pointed at a dead port → restart path taken
## (stubbed), exit 1 on honest post-restart verify failure

    $ SCHED_LIVE_URL=http://127.0.0.1:59998/api/v1/live \
      SCHED_RESTART_CMD="echo STUB-RESTART-COMMAND-INVOKED" \
      bash scripts/scheduler-watchdog.sh
    Scheduler unresponsive at 2026-09-27T00:18:24-05:00 (GET http://127.0.0.1:59998/api/v1/live -> HTTP 000 within 5s), restarting via systemd...
    STUB-RESTART-COMMAND-INVOKED
    FAILED: Scheduler did not come back up at 2026-09-27T00:18:29-05:00 (GET http://127.0.0.1:59998/api/v1/live -> HTTP 000)
    exit=1

    $ grep -c "STUB-RESTART-COMMAND-INVOKED" run-iii.out   # exactly one restart attempt
    1

## Run iv (edge) — /api/v1/live answers 404 (route unavailable) → legacy
## /projects liveness verdict, no restart

    $ SCHED_LIVE_URL=http://127.0.0.1:8642/no-such-route \
      SCHED_RESTART_CMD="echo STUB-RESTART-COMMAND-INVOKED" \
      bash scripts/scheduler-watchdog.sh
    NOTE: /api/v1/live is 404 (route unavailable in deployed binary); using legacy /projects liveness verdict (SCHED-GAP-1643).
    exit=0

## Invariants checked across all runs

- schedulerd pid 839395 listening on 127.0.0.1:9090 before AND after every
  run — the demos never restarted the real scheduler.
- Restart command invoked only in run iii (the restart-worthy case), and
  there exactly once, via the SCHED_RESTART_CMD stub.
- Exit codes: 0 healthy · 1 restart failed / 0-projects warning ·
  2 gateway dead while scheduler alive (no restart performed).

---

# SCHED-GAP-16971 — four-case decision evidence (BOTH DEAD branch)

Fault-injection battery (2026-11-08 ~04:59-05:01 -05), run against the
SCHED-GAP-16971 working-tree script. Every run used INJECTED endpoints and
STUB restart commands; the live scheduler (127.0.0.1:9090, systemd user unit
MainPID=3347652) and the live gateway (127.0.0.1:8642/health) were never
referenced by any run and were re-verified healthy AFTER the battery
(both still 200; scheduler unit MainPID unchanged 3347652 before/after).
`bash -n scripts/scheduler-watchdog.sh` → exit 0.

## Injected endpoints (stub HTTP servers, 127.0.0.1 only)

- Scheduler stub: `http://127.0.0.1:18901/api/v1/live` and
  `http://127.0.0.1:18901/api/v1/projects` → 200 when the stub server is
  running; connection refused (HTTP 000) when it is not.
- Gateway stub: `http://127.0.0.1:18942/health` → 200 when running,
  HTTP 000 when not.
- Stub server (`stub_server.py`): python3 http.server bound to
  127.0.0.1:<port>, role `scheduler` answers 200 on /api/v1/live and
  /api/v1/projects, role `gateway` answers 200 on /health, anything else 404:

```python
#!/usr/bin/env python3
import argparse
from http.server import BaseHTTPRequestHandler, HTTPServer

def main():
    p = argparse.ArgumentParser()
    p.add_argument("--port", type=int, required=True)
    p.add_argument("--role", choices=["scheduler", "gateway"], required=True)
    a = p.parse_args()
    class H(BaseHTTPRequestHandler):
        def do_GET(self):
            if a.role == "scheduler" and self.path.startswith("/api/v1/live"):
                self._respond(200, b'{"status":"ok"}')
            elif a.role == "scheduler" and self.path.startswith("/api/v1/projects"):
                self._respond(200, b'{"projects":[{"id":"stub-project"}]}')
            elif a.role == "gateway" and self.path.startswith("/health"):
                self._respond(200, b'{"status":"ok"}')
            else:
                self._respond(404, b"{}")
        def _respond(self, code, body):
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        def log_message(self, *args):
            pass
    HTTPServer(("127.0.0.1", a.port), H).serve_forever()

if __name__ == "__main__":
    main()
```

## Fake restart commands

- `SCHED_RESTART_CMD=<stubdir>/sched-restart-stub.sh` — appends
  `TS_NS=<epoch-ns> ROLE=sched argv=[...]` to a shared invocations log
  (nanosecond timestamps give an execution-order proof) and brings the stub
  scheduler endpoint up, unless `SCHED_STUB_RECOVERS=0`.
- `GATEWAY_RESTART_CMD=<stubdir>/gw-restart-stub.sh` — same shape with
  `ROLE=gw`; brings the stub gateway endpoint up unless
  `GW_STUB_RECOVERS=0` (restart command runs fine but the gateway STAYS
  down). Neither stub ever touches a live daemon or a systemd unit.

```bash
# sched-restart-stub.sh (gw-restart-stub.sh identical with ROLE=gw /
# GW_STUB_RECOVERS / STUB_GW_PORT / --role gateway)
#!/bin/bash
set -u
echo "TS_NS=$(date +%s%N) ROLE=sched argv=[$*]" >> "$STUB_LOG_DIR/invocations.log"
if [ "${SCHED_STUB_RECOVERS:-1}" = "1" ]; then
    setsid python3 "$STUB_DIR/stub_server.py" --port "$STUB_SCHED_PORT" --role scheduler \
        >> "$STUB_LOG_DIR/sched-server.log" 2>&1 &
    echo $! > "$STUB_LOG_DIR/sched-server.pid"
fi
exit "${SCHED_STUB_EXIT_CODE:-0}"
```

Every watchdog invocation set:
`SCHED_LIVE_URL=http://127.0.0.1:18901/api/v1/live
GATEWAY_HEALTH_URL=http://127.0.0.1:18942/health
SCHED_PROJECTS_URL=http://127.0.0.1:18901/api/v1/projects
SCHED_RESTART_CMD=.../sched-restart-stub.sh
GATEWAY_RESTART_CMD=.../gw-restart-stub.sh
GATEWAY_PROBE_ATTEMPTS=2 GATEWAY_PROBE_INTERVAL=1`
(fast probe knobs prove the env overrides; production defaults stay 3 x 15s).

## R0 — RED control: PRE-FIX script (git HEAD blob), both dead

Pre-fix behavior reproduced the 2026-11-03 13:41 incident shape: with both
stubs down (scheduler stub unrecoverable — a scheduler restarted into a dead
gateway does not come back), the old script never probed the gateway and
aimed its only restart at the scheduler anyway:

    Scheduler unresponsive at 2026-11-08T04:59:51-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000 within 5s), restarting via systemd...
    FAILED: Scheduler did not come back up at 2026-11-08T04:59:56-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000)
    exit=1

Scheduler-restart invocations: 1 · gateway-restart invocations: 0 ·
gateway probe line: ABSENT (restart aimed at a dead gateway).

## R1 — BOTH DEAD → gateway brought up FIRST, then scheduler; exit 0
## only when BOTH answer 200

    Scheduler unresponsive at 2026-11-08T04:59:56-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000 within 5s) — probing the gateway BEFORE any restart (SCHED-GAP-16971)...
    Gateway probe result: GET http://127.0.0.1:18942/health -> HTTP 000 after up to 2 probe(s) x 1s.
    ⚠️ BOTH DOWN: scheduler unresponsive AND gateway down. Bringing the GATEWAY up first (SCHED-GAP-16971: a scheduler restart into a dead gateway recovered nothing on 2026-11-03)...
    Gateway back up (GET http://127.0.0.1:18942/health -> HTTP 200) — restarting the scheduler...
    Both recovered at 2026-11-08T05:00:07-05:00: gateway http://127.0.0.1:18942/health -> HTTP 200 and scheduler http://127.0.0.1:18901/api/v1/live -> HTTP 200.
    exit=0

- Gateway-restart invocations: 1 · scheduler-restart invocations: 1.
- Execution order proof from the shared invocations log (nanosecond
  timestamps): gateway stub invoked BEFORE scheduler stub
  (TS_NS 1794131997775416826 < 1794132002792631768).
- Log-line order: the gateway probe result (HTTP 000) precedes the gateway
  restart line (line 2 < 3), and the scheduler restart is ordered only on a
  line that also carries the gateway's post-restart HTTP 200.
- After the run both stub endpoints answered 200 (exit-0 requires BOTH).

## R2 — GATEWAY DEAD, SCHEDULER ALIVE → alert, ZERO scheduler restarts,
## exit 2 (preserved from SCHED-GAP-1643)

    ⚠️ GATEWAY DOWN, SCHEDULER ALIVE — NOT restarting the scheduler (SCHED-GAP-1643). GET http://127.0.0.1:18942/health -> HTTP 000 after 2 probe(s) x 1s. The scheduler is provably live (http://127.0.0.1:18901/api/v1/live -> 200); its handlers may be slow while its spawns retry against the dead gateway. Restarting the scheduler here is what turned the 2026-09-26 gateway outage into 163 failed ticks. Attend to the gateway, not the scheduler.
    exit=2

Scheduler-restart invocations: 0 · gateway-restart invocations: 0.

## R3 — SCHEDULER DEAD, GATEWAY ALIVE → exactly ONE verified restart,
## exit 0 (preserved; gateway probe now logged before the restart line)

    Scheduler unresponsive at 2026-11-08T05:00:10-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000 within 5s) — probing the gateway BEFORE any restart (SCHED-GAP-16971)...
    Gateway probe result: GET http://127.0.0.1:18942/health -> HTTP 200 after up to 2 probe(s) x 1s.
    Gateway is up (GET http://127.0.0.1:18942/health -> HTTP 200); the scheduler is the blocker — restarting via systemd...
    Scheduler restarted successfully at 2026-11-08T05:00:15-05:00
    exit=0

Scheduler-restart invocations: exactly 1 · gateway-restart invocations: 0 ·
gateway probe result line (HTTP 200) precedes the restart line (2 < 3).

## R4 — BOTH DEAD, gateway restart cannot recover → exit 1, GATEWAY named
## as the blocker, scheduler restart SKIPPED

    Scheduler unresponsive at 2026-11-08T05:00:16-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000 within 5s) — probing the gateway BEFORE any restart (SCHED-GAP-16971)...
    Gateway probe result: GET http://127.0.0.1:18942/health -> HTTP 000 after up to 2 probe(s) x 1s.
    ⚠️ BOTH DOWN: scheduler unresponsive AND gateway down. Bringing the GATEWAY up first (SCHED-GAP-16971: a scheduler restart into a dead gateway recovered nothing on 2026-11-03)...
    FAILED: GATEWAY is the blocker — still down after gateway restart at 2026-11-08T05:00:22-05:00 (GET http://127.0.0.1:18942/health -> HTTP 000). Scheduler restart SKIPPED (never restart into a dead gateway).
    exit=1

Gateway-restart invocations: 1 · scheduler-restart invocations: 0
(GW_STUB_RECOVERS=0: the restart command ran, the gateway stayed down, and
the scheduler was NOT restarted into it).

## R5 — BOTH DEAD, gateway recovers but scheduler restart does NOT →
## exit 1, SCHEDULER named as the blocker

    Scheduler unresponsive at 2026-11-08T05:00:22-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000 within 5s) — probing the gateway BEFORE any restart (SCHED-GAP-16971)...
    Gateway probe result: GET http://127.0.0.1:18942/health -> HTTP 000 after up to 2 probe(s) x 1s.
    ⚠️ BOTH DOWN: scheduler unresponsive AND gateway down. Bringing the GATEWAY up first (SCHED-GAP-16971: a scheduler restart into a dead gateway recovered nothing on 2026-11-03)...
    Gateway back up (GET http://127.0.0.1:18942/health -> HTTP 200) — restarting the scheduler...
    FAILED: SCHEDULER is the blocker — gateway is up (HTTP 200) but the scheduler did not come back up at 2026-11-08T05:00:34-05:00 (GET http://127.0.0.1:18901/api/v1/live -> HTTP 000).
    exit=1

Gateway-restart invocations: 1 · scheduler-restart invocations: 1
(SCHED_STUB_RECOVERS=0).

## R6 — BOTH UP (healthy regression) → silent, exit 0, zero restarts

    exit=0   (no output)

Scheduler-restart invocations: 0 · gateway-restart invocations: 0.

## Invariants checked across the battery (all asserted by the driver)

- `bash -n scripts/scheduler-watchdog.sh` → exit 0.
- A line naming the gateway probe result precedes every restart line in
  every run that restarts anything (R1, R3, R4, R5).
- No restart was ever aimed at a gateway that had not just answered 200:
  R1/R5's scheduler restart is ordered on a line carrying the gateway's
  HTTP 200; R4 skips the scheduler restart entirely while the gateway is
  down; R2 (scheduler alive) restarts nothing.
- Exit codes: 0 both-up / verified recovery (BOTH endpoints 200) ·
  1 restart failed or 0-projects warning, blocker named (gateway vs
  scheduler) · 2 gateway dead while scheduler alive (no restart).
- Live safety: every run used the injected 18901/18942 URLs; live 9090 and
  8642 answered 200 before and after; scheduler unit MainPID 3347652 before
  and after. The live `watchdog.timer`/`watchdog.service` unit executes
  `/home/kara/.hermes/scripts/watchdog.sh` (a different, larger deployed
  ops script) — this repo change cannot affect the live timer until it is
  deployed through whatever path syncs it.

Harness location at run time:
`/home/kara/.hermes/cache/scratch/sched-gap-16971/` (scratch — recreated
from this section as needed: stub_server.py, sched-restart-stub.sh,
gw-restart-stub.sh, and a driver that asserts every invariant above and
exits non-zero on any failure).
