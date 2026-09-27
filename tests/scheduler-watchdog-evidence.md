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
