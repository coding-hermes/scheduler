#!/bin/bash
# Scheduler daemon watchdog — decides whether the SCHEDULER itself is dead
# before restarting it, and never restarts the scheduler for a GATEWAY outage.
# Runs every 2 minutes (watchdog.timer → watchdog.service).
#
# Three-case decision (SCHED-GAP-1643):
#   a. SCHEDULER DEAD — /api/v1/live (DB-free, answers <10ms from process
#      memory, never queues; landed 95516e44) does not answer 200 → the
#      scheduler itself is unresponsive → restart IS warranted:
#      `systemctl --user restart coding-hermes-scheduler` (NEVER pkill —
#      pkill triggers systemd's auto-respawn with the unit's flags and the
#      manual launch loses the port race; h3 re-drift incident 2026-08-07).
#   b. SCHEDULER ALIVE, GATEWAY DEAD — /api/v1/live is 200 but the gateway
#      (127.0.0.1:8642/health) stays down after 3 probes over ~30s (retries
#      avoid racing a gateway restart) → DO NOT restart. A gateway outage
#      makes the scheduler's own handlers slow/fail via its spawn retries
#      ("WARN: slow request handler=health step=countActiveTicks"), which the
#      old /projects-only check misread as a dead scheduler. On 2026-09-26
#      23:41-23:50 UTC that false positive restarted the healthy scheduler
#      mid-outage (NRestarts=0 + Restart=on-failure = the restart was
#      EXPLICIT, not a crash) and the fresh daemon resumed orphaned ticks
#      against the still-dead gateway — 163 failed ticks over hours instead
#      of a 20-minute blip. This case now says so loudly and exits non-zero
#      (alert-worthy) WITHOUT restarting anything.
#   c. ALIVE but 0 projects — warning path (wrong DB path?), reached ONLY
#      through the live-route gate: a slow/queueing /projects must never
#      restart a provably-live daemon (same spirit as the 2026-09-21
#      false-CRITICAL repair in watchdog.sh — DB-backed endpoints queue on
#      the single serialized SQLite connection; /api/v1/live never does).
#
# Edge: HTTP 404 on /api/v1/live = ROUTE-UNAVAILABLE (deployed binary
# predates 95516e44, cf. watchdog.sh 2026-09-22 note) — fall back to the
# legacy /projects verdict instead of declaring a live daemon dead on a
# missing probe.
#
# Every probe target and the restart command is env-overridable (the
# watchdog.sh SCHEDULER/STATE_FILE pattern) so each branch is testable
# against dead ports without touching live infra:
#   SCHED_LIVE_URL        (default http://127.0.0.1:9090/api/v1/live)
#   GATEWAY_HEALTH_URL    (default http://127.0.0.1:8642/health)
#   SCHED_PROJECTS_URL    (default http://127.0.0.1:9090/api/v1/projects)
#   SCHED_RESTART_CMD     (default systemctl --user restart coding-hermes-scheduler)
# Exit codes: 0 healthy/verified restart · 1 restart failed or 0-projects
# warning · 2 gateway dead while scheduler alive (no restart performed).

SCHED_LIVE_URL="${SCHED_LIVE_URL:-http://127.0.0.1:9090/api/v1/live}"
GATEWAY_HEALTH_URL="${GATEWAY_HEALTH_URL:-http://127.0.0.1:8642/health}"
SCHED_PROJECTS_URL="${SCHED_PROJECTS_URL:-http://127.0.0.1:9090/api/v1/projects}"
SCHED_RESTART_CMD="${SCHED_RESTART_CMD:-systemctl --user restart coding-hermes-scheduler}"

LIVE_TIMEOUT=5            # /api/v1/live is DB-free (<10ms); 5s is ~500x headroom
GATEWAY_PROBE_ATTEMPTS="${GATEWAY_PROBE_ATTEMPTS:-3}"
GATEWAY_PROBE_INTERVAL="${GATEWAY_PROBE_INTERVAL:-15}"  # secs between probes; 3 probes over ~30s
GATEWAY_PROBE_TIMEOUT=5
PROJECTS_TIMEOUT=25       # DB-backed handler; must exceed the pool busy_timeout (5s)
RESTART_SETTLE_SECS=5

http_code() { # url timeout → HTTP code ("000" = refused/timeout/no answer)
    curl -s -o /dev/null -w "%{http_code}" --max-time "$2" "$1" 2>/dev/null
}

restart_scheduler() {
    # Intentional word splitting: SCHED_RESTART_CMD is a full command line by
    # design (test runs override it with an `echo` stub).
    # shellcheck disable=SC2086
    $SCHED_RESTART_CMD
}

project_count() { # → count of projects, or empty when payload unreadable
    curl -s --max-time "$PROJECTS_TIMEOUT" "$SCHED_PROJECTS_URL" 2>/dev/null \
        | python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
    print(len(d.get("projects", [])))
except Exception:
    print("")' 2>/dev/null
}

LIVE=$(http_code "$SCHED_LIVE_URL" "$LIVE_TIMEOUT")

# ── Liveness gate: is the scheduler itself provably alive? ──
ALIVE=0
if [ "$LIVE" = "200" ]; then
    ALIVE=1
elif [ "$LIVE" = "404" ]; then
    # Route unavailable in the deployed binary: legacy /projects verdict.
    PROJ_CODE=$(http_code "$SCHED_PROJECTS_URL" "$PROJECTS_TIMEOUT")
    if [ "$PROJ_CODE" = "200" ]; then
        ALIVE=1
        echo "NOTE: /api/v1/live is 404 (route unavailable in deployed binary); using legacy /projects liveness verdict (SCHED-GAP-1643)."
    fi
fi

# ── Case a: scheduler unresponsive → restart is warranted ──
if [ "$ALIVE" -ne 1 ]; then
    echo "Scheduler unresponsive at $(date -Iseconds) (GET $SCHED_LIVE_URL -> HTTP $LIVE within ${LIVE_TIMEOUT}s), restarting via systemd..."
    restart_scheduler
    sleep "$RESTART_SETTLE_SECS"
    LIVE2=$(http_code "$SCHED_LIVE_URL" "$LIVE_TIMEOUT")
    if [ "$LIVE2" = "200" ]; then
        echo "Scheduler restarted successfully at $(date -Iseconds)"
        exit 0
    fi
    echo "FAILED: Scheduler did not come back up at $(date -Iseconds) (GET $SCHED_LIVE_URL -> HTTP $LIVE2)"
    exit 1
fi

# ── Case b gate: scheduler alive — is the gateway alive? ──
GW=""
i=1
while [ "$i" -le "$GATEWAY_PROBE_ATTEMPTS" ]; do
    GW=$(http_code "$GATEWAY_HEALTH_URL" "$GATEWAY_PROBE_TIMEOUT")
    [ "$GW" = "200" ] && break
    [ "$i" -lt "$GATEWAY_PROBE_ATTEMPTS" ] && sleep "$GATEWAY_PROBE_INTERVAL"
    i=$((i + 1))
done

if [ "$GW" != "200" ]; then
    echo "⚠️ GATEWAY DOWN, SCHEDULER ALIVE — NOT restarting the scheduler (SCHED-GAP-1643). GET $GATEWAY_HEALTH_URL -> HTTP $GW after $GATEWAY_PROBE_ATTEMPTS probe(s) x ${GATEWAY_PROBE_INTERVAL}s. The scheduler is provably live ($SCHED_LIVE_URL -> 200); its handlers may be slow while its spawns retry against the dead gateway. Restarting the scheduler here is what turned the 2026-09-26 gateway outage into 163 failed ticks. Attend to the gateway, not the scheduler."
    exit 2
fi

# ── Case c: alive (scheduler + gateway) — project-count warning path ──
COUNT=$(project_count)
if [ -z "$COUNT" ]; then
    # Payload unreadable while the daemon is provably live: slow/queueing
    # handler, not an outage. Never restart a live daemon over it.
    echo "NOTE: scheduler live + gateway healthy, but $SCHED_PROJECTS_URL gave no readable payload — not restarting a provably-live daemon (SCHED-GAP-1643 gate); re-checking next cycle."
    exit 0
fi
if [ "$COUNT" = "0" ]; then
    echo "WARNING: Scheduler alive but 0 projects loaded (wrong DB path?) at $(date -Iseconds)"
    exit 1
fi

# All checks passed — silent (healthy watchdog stays quiet).
exit 0
