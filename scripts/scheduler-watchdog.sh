#!/bin/bash
# Scheduler daemon watchdog — decides whether the SCHEDULER itself is dead
# before restarting it, and never restarts the scheduler for a GATEWAY outage.
# Runs every 2 minutes (watchdog.timer → watchdog.service).
#
# Four-case decision (SCHED-GAP-1643 + SCHED-GAP-16971):
#   a. SCHEDULER DEAD, GATEWAY ALIVE — /api/v1/live (DB-free, answers <10ms
#      from process memory, never queues; landed 95516e44) does not answer
#      200 while the gateway health probe does → the scheduler itself is
#      unresponsive → restart IS warranted:
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
#      Case c also reads the /api/v1/health PAYLOAD (SCHEDGAP-000001): when
#      the scheduler and gateway are both up but the payload's `db` field is
#      present and != "connected", an alert fires and the script exits
#      non-zero WITHOUT restarting the provably-live daemon — never restart
#      a live daemon over a DB-backed probe (2026-09-21 false-CRITICAL
#      guard). An unreadable health payload keeps the case-c NOTE behavior.
#   d. BOTH DEAD (SCHED-GAP-16971) — /api/v1/live fails AND the gateway
#      health probe fails → the gateway is brought up FIRST and verified,
#      and only then is the scheduler restarted. On 2026-11-03 13:41 the
#      watchdog saw /api/v1/live -> HTTP 000, restarted the scheduler into
#      the simultaneously-dead gateway, and 24s later reported "FAILED:
#      Scheduler did not come back up" — a restart aimed at a dead gateway
#      cannot recover service. This branch orders gateway-before-scheduler,
#      skips the scheduler restart entirely while the gateway is still down,
#      and exits 0 only when BOTH endpoints answer 200, naming whichever one
#      stayed down as the blocker when they don't.
#
# Edge: HTTP 404 on /api/v1/live = ROUTE-UNAVAILABLE (deployed binary
# predates 95516e44, cf. watchdog.sh 2026-09-22 note) — fall back to the
# legacy /projects verdict instead of declaring a live daemon dead on a
# missing probe.
#
# Every probe target and restart command is env-overridable (the
# watchdog.sh SCHEDULER/STATE_FILE pattern) so each branch is testable
# against dead ports without touching live infra:
#   SCHED_LIVE_URL        (default http://127.0.0.1:9090/api/v1/live)
#   GATEWAY_HEALTH_URL    (default http://127.0.0.1:8642/health)
#   SCHED_PROJECTS_URL    (default http://127.0.0.1:9090/api/v1/projects)
#   SCHED_HEALTH_URL      (default http://127.0.0.1:9090/api/v1/health)
#   SCHEDULER             (back-compat alias, deployed runtime / gate arms:
#                          base URL that seeds SCHED_LIVE_URL / SCHED_PROJECTS_URL /
#                          SCHED_HEALTH_URL when they are unset; only
#                          SCHEDULER=<base> is honored, and the per-URL vars win)
#   STATE_FILE            (accepted for deployed-runtime compatibility; unused —
#                          this verdict is stateless, see the env block below)
#   SCHED_RESTART_CMD     (default systemctl --user restart coding-hermes-scheduler)
#   GATEWAY_RESTART_CMD   (default systemctl --user restart
#                          coding-hermes-scheduler-gateway) — reached ONLY
#                          from the both-dead branch; a tick where the
#                          gateway answered 200 never runs it.
# Exit codes: 0 healthy/verified restart (incl. both-dead recovery with BOTH
# endpoints answering 200) · 1 restart failed or 0-projects warning or db
# != "connected" alert (a both-dead failure names its blocker: gateway or
# scheduler) · 2 gateway dead while scheduler alive (no restart performed).

SCHEDULER_BASE="${SCHEDULER:-}"   # back-compat alias (deployed runtime / gate arms): base URL seeding the three scheduler probe URLs below; GATEWAY_HEALTH_URL keeps its own default
if [ -n "$SCHEDULER_BASE" ]; then
    SCHED_LIVE_URL="${SCHED_LIVE_URL:-$SCHEDULER_BASE/api/v1/live}"
    SCHED_PROJECTS_URL="${SCHED_PROJECTS_URL:-$SCHEDULER_BASE/api/v1/projects}"
    SCHED_HEALTH_URL="${SCHED_HEALTH_URL:-$SCHEDULER_BASE/api/v1/health}"
fi
SCHED_LIVE_URL="${SCHED_LIVE_URL:-http://127.0.0.1:9090/api/v1/live}"
GATEWAY_HEALTH_URL="${GATEWAY_HEALTH_URL:-http://127.0.0.1:8642/health}"
SCHED_PROJECTS_URL="${SCHED_PROJECTS_URL:-http://127.0.0.1:9090/api/v1/projects}"
SCHED_HEALTH_URL="${SCHED_HEALTH_URL:-http://127.0.0.1:9090/api/v1/health}"
SCHED_RESTART_CMD="${SCHED_RESTART_CMD:-systemctl --user restart coding-hermes-scheduler}"
GATEWAY_RESTART_CMD="${GATEWAY_RESTART_CMD:-systemctl --user restart coding-hermes-scheduler-gateway}"
# STATE_FILE (deployed-runtime convention) is accepted but unused: the
# four-case verdict is stateless — each run re-probes from scratch instead of
# tracking consecutive-failure streaks (the old script's streak file is what
# silently rode out the 11-23 5.6h wedge).

LIVE_TIMEOUT=5            # /api/v1/live is DB-free (<10ms); 5s is ~500x headroom
GATEWAY_PROBE_ATTEMPTS="${GATEWAY_PROBE_ATTEMPTS:-3}"
GATEWAY_PROBE_INTERVAL="${GATEWAY_PROBE_INTERVAL:-15}"  # secs between probes; 3 probes over ~30s
GATEWAY_PROBE_TIMEOUT=5
PROJECTS_TIMEOUT=25       # DB-backed handler; must exceed the pool busy_timeout (5s)
HEALTH_TIMEOUT=25         # DB-backed handler; same class as /projects (must exceed the pool busy_timeout)
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

restart_gateway() {
    # Same contract as restart_scheduler: GATEWAY_RESTART_CMD is a full
    # command line by design (test runs override it with a stub). This is
    # reached ONLY from the both-dead branch (case d) — never on a tick
    # where the gateway probe answered 200.
    # shellcheck disable=SC2086
    $GATEWAY_RESTART_CMD
}

probe_gateway() { # → final HTTP code after GATEWAY_PROBE_ATTEMPTS tries
    local gw="" i=1
    while [ "$i" -le "$GATEWAY_PROBE_ATTEMPTS" ]; do
        gw=$(http_code "$GATEWAY_HEALTH_URL" "$GATEWAY_PROBE_TIMEOUT")
        [ "$gw" = "200" ] && break
        [ "$i" -lt "$GATEWAY_PROBE_ATTEMPTS" ] && sleep "$GATEWAY_PROBE_INTERVAL"
        i=$((i + 1))
    done
    printf '%s' "$gw"
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

sched_db_status() { # → value of the health payload's `db` field, or empty when unreadable/absent
    curl -s --max-time "$HEALTH_TIMEOUT" "$SCHED_HEALTH_URL" 2>/dev/null \
        | python3 -c 'import json,sys
try:
    d = json.load(sys.stdin)
    if "db" in d:
        print(str(d["db"]))
    else:
        print("")
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

# ── Case a/d gate: scheduler unresponsive → probe the gateway BEFORE any ──
# ── restart (SCHED-GAP-16971: the gateway decides what a restart may do) ──
if [ "$ALIVE" -ne 1 ]; then
    echo "Scheduler unresponsive at $(date -Iseconds) (GET $SCHED_LIVE_URL -> HTTP $LIVE within ${LIVE_TIMEOUT}s) — probing the gateway BEFORE any restart (SCHED-GAP-16971)..."
    GW=$(probe_gateway)
    echo "Gateway probe result: GET $GATEWAY_HEALTH_URL -> HTTP $GW after up to $GATEWAY_PROBE_ATTEMPTS probe(s) x ${GATEWAY_PROBE_INTERVAL}s."

    if [ "$GW" != "200" ]; then
        # ── Case d: BOTH DEAD — gateway first, scheduler only once it answers ──
        echo "⚠️ BOTH DOWN: scheduler unresponsive AND gateway down. Bringing the GATEWAY up first (SCHED-GAP-16971: a scheduler restart into a dead gateway recovered nothing on 2026-11-03)..."
        restart_gateway
        sleep "$RESTART_SETTLE_SECS"
        GW2=$(http_code "$GATEWAY_HEALTH_URL" "$GATEWAY_PROBE_TIMEOUT")
        if [ "$GW2" != "200" ]; then
            echo "FAILED: GATEWAY is the blocker — still down after gateway restart at $(date -Iseconds) (GET $GATEWAY_HEALTH_URL -> HTTP $GW2). Scheduler restart SKIPPED (never restart into a dead gateway)."
            exit 1
        fi
        echo "Gateway back up (GET $GATEWAY_HEALTH_URL -> HTTP $GW2) — restarting the scheduler..."
        restart_scheduler
        sleep "$RESTART_SETTLE_SECS"
        LIVE2=$(http_code "$SCHED_LIVE_URL" "$LIVE_TIMEOUT")
        GW3=$(http_code "$GATEWAY_HEALTH_URL" "$GATEWAY_PROBE_TIMEOUT")
        if [ "$LIVE2" = "200" ] && [ "$GW3" = "200" ]; then
            echo "Both recovered at $(date -Iseconds): gateway $GATEWAY_HEALTH_URL -> HTTP 200 and scheduler $SCHED_LIVE_URL -> HTTP 200."
            exit 0
        fi
        if [ "$GW3" != "200" ]; then
            echo "FAILED: GATEWAY is the blocker — dropped again after the recovery attempt at $(date -Iseconds) (GET $GATEWAY_HEALTH_URL -> HTTP $GW3); scheduler probe: GET $SCHED_LIVE_URL -> HTTP $LIVE2."
        else
            echo "FAILED: SCHEDULER is the blocker — gateway is up (HTTP $GW3) but the scheduler did not come back up at $(date -Iseconds) (GET $SCHED_LIVE_URL -> HTTP $LIVE2)."
        fi
        exit 1
    fi

    # ── Case a: gateway is up — the scheduler is the sole blocker ──
    echo "Gateway is up (GET $GATEWAY_HEALTH_URL -> HTTP $GW); the scheduler is the blocker — restarting via systemd..."
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
GW=$(probe_gateway)

if [ "$GW" != "200" ]; then
    echo "⚠️ GATEWAY DOWN, SCHEDULER ALIVE — NOT restarting the scheduler (SCHED-GAP-1643). GET $GATEWAY_HEALTH_URL -> HTTP $GW after $GATEWAY_PROBE_ATTEMPTS probe(s) x ${GATEWAY_PROBE_INTERVAL}s. The scheduler is provably live ($SCHED_LIVE_URL -> 200); its handlers may be slow while its spawns retry against the dead gateway. Restarting the scheduler here is what turned the 2026-09-26 gateway outage into 163 failed ticks. Attend to the gateway, not the scheduler."
    exit 2
fi

# ── Case c: alive (scheduler + gateway) — db-health + project-count paths ──

# DB-backed health payload (SCHEDGAP-000001): a live daemon whose health
# endpoint says db != "connected" is alert-worthy, but NEVER a restart —
# DB-backed endpoints queue on the serialized SQLite connection (2026-09-21
# false-CRITICAL guard), so a live daemon must not be killed over one. An
# unreadable/absent payload keeps the case-c NOTE behavior (no alert).
DB=$(sched_db_status)
if [ -z "$DB" ]; then
    echo "NOTE: scheduler live + gateway healthy, but $SCHED_HEALTH_URL gave no readable payload (or no db field) — not restarting a provably-live daemon (2026-09-21 false-CRITICAL gate); re-checking next cycle."
    exit 0
fi
if [ "$DB" != "connected" ]; then
    echo "⚠️ SCHEDULER ALIVE BUT DB NOT CONNECTED — GET $SCHED_HEALTH_URL -> {\"db\":\"$DB\"} at $(date -Iseconds). NOT restarting the provably-live daemon (a DB-backed probe must never restart a live daemon; 2026-09-21 false-CRITICAL guard). Attend to the scheduler's database path/connection."
    exit 1
fi

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
