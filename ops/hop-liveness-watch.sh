#!/usr/bin/env bash
#
# hop-liveness-watch.sh — watch the gating hops of the code factory from a
# timer, so a hop that dies politely is noticed without a human running the
# chain probe (SCHED-GAP-16946).
#
# WHY THIS EXISTS
#   crier-fleet.service exited with status=0/SUCCESS at 2026-10-28T23:03 and sat
#   inactive for 11 hours. `Restart=on-failure` never applied to a clean exit,
#   no alert fired, and the only thing that noticed was a human running
#   ops/cluster-chain-probe.sh by hand. A hop that dies politely is
#   indistinguishable from a hop that was never needed — the same "green but did
#   nothing" failure class as a tick that exits 0 having done no work.
#
# WHAT IT DOES
#   For every hop in the table below it reads the unit's own state from systemd
#   (LoadState, ActiveState, SubState, Result, ExecMainCode, ExecMainStatus and
#   the state-change timestamps) and classifies it:
#
#     up          ActiveState=active
#     down        ActiveState=inactive or failed — the hop is not answering
#     transition  activating/deactivating/reloading — recorded, never alerted
#     unknown     LoadState=not-found — a named refusal, never silence
#
#   On a TRANSITION to down it appends one JSON record and prints one loud line
#   naming the hop, the unit, the exit status and how long it has been down.
#   While a hop stays down it re-alerts at most once per --re-alert interval
#   (default 3600s) so an unread first alert cannot decay into silence. On the
#   transition back to up it writes a recovery record.
#
#   Silent when every hop is up: no output, exit 0. This is a detector, not a
#   repairer — it never restarts, stops or reconfigures anything, so it is safe
#   to run against the live fleet at any time and can never fail-loop.
#
# USAGE
#   ops/hop-liveness-watch.sh [options]
#
#     --state PATH        state file (default ~/.hermes/logs/hop-liveness-state.tsv)
#     --alerts PATH       append-only alert log
#                         (default ~/.hermes/logs/hop-liveness-alerts.jsonl)
#     --re-alert SECS     re-alert interval while a hop stays down (default 3600)
#     --hop NAME          restrict to one hop (repeatable; default: every hop)
#     --json              print ONE summary object on stdout instead of the loud
#                         lines (the alert log is still appended)
#     --strict            exit 2 when any hop is down or unknown
#     --verbose           print one line per hop even when everything is up
#     -h | --help
#
#   ENV equivalents (for unit files and cron):
#     HOP_WATCH_STATE, HOP_WATCH_ALERTS, HOP_WATCH_REALERT, HOP_WATCH_HOPS,
#     HOP_WATCH_STRICT
#
#   HOP_WATCH_HOPS overrides the table, one `hop:scope:unit` per space — used by
#   the selftest and by an operator adding a hop before editing this file.
#
# EXIT CODES
#   0  ran; state reported (a down hop is a finding, not a fault of the watcher)
#   2  --strict and at least one hop is down or unknown
#   3  the watcher itself could not run (no systemctl, unwritable state dir)
#
# WHAT THIS DOES NOT COVER — see docs/hop-liveness-watchdog.md, which is the
# authority. In short: it watches UNIT LIVENESS on this box only. It does not
# prove a hop answers (that is the cluster-chain probe's job, hop by hop), it
# does not restart anything, and nothing outside this host watches this host.

set -uo pipefail

SCHEMA="hop-liveness/1"

STATE="${HOP_WATCH_STATE:-$HOME/.hermes/logs/hop-liveness-state.tsv}"
ALERTS="${HOP_WATCH_ALERTS:-$HOME/.hermes/logs/hop-liveness-alerts.jsonl}"
REALERT="${HOP_WATCH_REALERT:-3600}"
HOPS="${HOP_WATCH_HOPS:-router:user:task-router-proxy.service crier:user:crier-fleet.service scheduler:user:coding-hermes-scheduler.service bunker:system:bunkerd.service}"
STRICT="${HOP_WATCH_STRICT:-0}"
JSON=0
VERBOSE=0
FILTER=""

die() { printf 'hop-liveness-watch: %s\n' "$1" >&2; exit 3; }

usage() {
  sed -n '/^# USAGE/,/^# EXIT CODES/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; s/^#$//'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --state)    STATE="${2:?--state needs a path}"; shift 2 ;;
    --alerts)   ALERTS="${2:?--alerts needs a path}"; shift 2 ;;
    --re-alert) REALERT="${2:?--re-alert needs seconds}"; shift 2 ;;
    --hop)      FILTER="${FILTER} ${2:?--hop needs a name}"; shift 2 ;;
    --json)     JSON=1; shift ;;
    --strict)   STRICT=1; shift ;;
    --verbose)  VERBOSE=1; shift ;;
    -h|--help)  usage; exit 0 ;;
    *)          printf 'hop-liveness-watch: unknown argument %s\n' "$1" >&2; exit 2 ;;
  esac
done

command -v systemctl >/dev/null 2>&1 || die "systemctl not found — cannot read unit state (never reported as up)"

mkdir -p "$(dirname "$STATE")" "$(dirname "$ALERTS")" 2>/dev/null \
  || die "cannot create state/alert directory"

# One watcher at a time. A timer tick that overlaps a manual run must not
# double-alert; the run that took the lock owns the transition.
if command -v flock >/dev/null 2>&1; then
  exec 9>"${STATE}.lock" || die "cannot open lock file ${STATE}.lock"
  flock -n 9 || { [ "$VERBOSE" = 1 ] && echo "hop-liveness-watch: another run holds the lock; skipping"; exit 0; }
fi

now_epoch=$(date -u +%s)
now_iso=$(date -u +%Y-%m-%dT%H:%M:%SZ)
up_s=$(awk '{printf "%d", $1}' /proc/uptime 2>/dev/null || echo 0)

humanise() { # seconds -> 3d4h / 3h41m / 41m / 9s / "" for unknown
  local s="${1:-}"
  [ -z "$s" ] && { printf ''; return 0; }
  [ "$s" -lt 0 ] 2>/dev/null && s=0
  local d=$(( s / 86400 )) h=$(( (s % 86400) / 3600 )) m=$(( (s % 3600) / 60 )) sec=$(( s % 60 ))
  if   [ "$d" -gt 0 ]; then printf '%dd%dh' "$d" "$h"
  elif [ "$h" -gt 0 ]; then printf '%dh%dm' "$h" "$m"
  elif [ "$m" -gt 0 ]; then printf '%dm' "$m"
  else printf '%ds' "$sec"; fi
}

# ---- previous state ---------------------------------------------------------
declare -A PREV_STATUS=() PREV_ALERT=() PREV_DOWN_S=()
if [ -r "$STATE" ]; then
  while IFS=$'\t' read -r h st al ds; do
    if [ -z "${h:-}" ] || [ "$h" = "hop" ]; then continue; fi
    PREV_STATUS["$h"]="${st:-}"
    PREV_ALERT["$h"]="${al:-0}"
    PREV_DOWN_S["$h"]="${ds:-}"
  done < "$STATE"
fi

# ---- classification ---------------------------------------------------------
# Reads systemd's own view of one unit and sets HOP_STATUS / HOP_REASON /
# HOP_EXIT / HOP_RESULT / HOP_DOWN_S / HOP_SINCE_ISO / HOP_STATE_WORD.
classify() { # scope unit
  local scope="$1" unit="$2"
  local props load active sub result code status_val mono ts e

  if [ "$scope" = "user" ]; then
    props=$(systemctl --user show "$unit" \
      -p LoadState -p ActiveState -p SubState -p Result -p ExecMainCode \
      -p ExecMainStatus -p StateChangeTimestampMonotonic -p StateChangeTimestamp 2>/dev/null)
  else
    props=$(systemctl show "$unit" \
      -p LoadState -p ActiveState -p SubState -p Result -p ExecMainCode \
      -p ExecMainStatus -p StateChangeTimestampMonotonic -p StateChangeTimestamp 2>/dev/null)
  fi

  load=""; active=""; sub=""; result=""; code=""; status_val=""; mono=""; ts=""
  while IFS='=' read -r k v; do
    case "$k" in
      LoadState) load="$v" ;;
      ActiveState) active="$v" ;;
      SubState) sub="$v" ;;
      Result) result="$v" ;;
      ExecMainCode) code="$v" ;;
      ExecMainStatus) status_val="$v" ;;
      StateChangeTimestampMonotonic) mono="$v" ;;
      StateChangeTimestamp) ts="$v" ;;
    esac
  done <<< "$props"

  HOP_STATE_WORD="${sub:-${active:-unknown}}"
  HOP_EXIT="${status_val:--}"
  HOP_RESULT="${result:-unknown}"
  HOP_CODE="${code:--}"
  case "$code" in
    0) HOP_EXIT_KIND="running" ;;   # no ExecMain exit recorded (unit is up)
    1) HOP_EXIT_KIND="exited" ;;
    2) HOP_EXIT_KIND="killed" ;;
    3) HOP_EXIT_KIND="dumped" ;;
    4) HOP_EXIT_KIND="watchdog" ;;
    *) HOP_EXIT_KIND="unknown" ;;
  esac
  HOP_SINCE_ISO=""
  HOP_DOWN_S=""

  if [ -z "$load" ]; then
    HOP_STATUS="unknown"; HOP_REASON="HOP_UNIT_UNREADABLE"; return 0
  fi
  if [ "$load" = "not-found" ]; then
    HOP_STATUS="unknown"; HOP_REASON="HOP_UNIT_NOT_FOUND"; return 0
  fi

  case "$active" in
    active)
      HOP_STATUS="up"; HOP_REASON="HOP_UP"; return 0 ;;
    failed)
      if [ "$result" = "success" ] || [ "$status_val" = "0" ]; then
        HOP_STATUS="down"; HOP_REASON="HOP_DOWN_CLEAN_EXIT"
      else
        HOP_STATUS="down"; HOP_REASON="HOP_DOWN_FAILURE"
      fi ;;
    inactive)
      # MUTATION POINT (selftest): the clean-exit branch the 11h outage fell in.
      if [ "$result" = "success" ] || [ "$status_val" = "0" ]; then
        HOP_STATUS="down"; HOP_REASON="HOP_DOWN_CLEAN_EXIT"
      else
        HOP_STATUS="down"; HOP_REASON="HOP_DOWN_FAILURE"
      fi ;;
    activating|deactivating|reloading|refreshing)
      HOP_STATUS="transition"; HOP_REASON="HOP_TRANSITION"; return 0 ;;
    *)
      HOP_STATUS="unknown"; HOP_REASON="HOP_UNKNOWN_STATE"; return 0 ;;
  esac

  # How long it has been down: prefer systemd's MONOTONIC state-change stamp
  # against /proc/uptime (immune to timezone and wall-clock changes), fall back
  # to the wall-clock stamp, and report nothing rather than assert a number.
  if [ -n "$mono" ] && [ "$mono" != "n/a" ] && [ "$mono" -gt 0 ] 2>/dev/null; then
    HOP_DOWN_S=$(( up_s - mono / 1000000 ))
    [ "$HOP_DOWN_S" -lt 0 ] && HOP_DOWN_S=0
    HOP_SINCE_ISO=$(date -u -d "@$(( now_epoch - HOP_DOWN_S ))" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)
  elif [ -n "$ts" ] && [ "$ts" != "n/a" ]; then
    if e=$(date -d "$ts" +%s 2>/dev/null); then
      HOP_DOWN_S=$(( now_epoch - e ))
      [ "$HOP_DOWN_S" -lt 0 ] && HOP_DOWN_S=0
      HOP_SINCE_ISO=$(date -u -d "@$e" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)
    fi
  fi
  return 0
}

# ---- emission ---------------------------------------------------------------
# append_alert hop event unit state reason exit_code exit_kind result down_s since repeat
append_alert() {
  local hop="$1" event="$2" unit="$3" st="$4" reason="$5" ec="$6" kind="$7" res="$8"
  local ds="${9:-}" since="${10:-}" rep="${11:-false}"
  local ds_json="null" since_json="null"
  if [ -n "$ds" ]; then ds_json="$ds"; fi
  if [ -n "$since" ]; then since_json="\"$since\""; fi
  printf '{"schema":"%s","event":"%s","hop":"%s","unit":"%s","state":"%s","reason":"%s","exit_code":"%s","exit_kind":"%s","result":"%s","down_s":%s,"down_for":"%s","since":%s,"at":"%s","repeat":%s,"watchdog":"ops/hop-liveness-watch.sh"}\n' \
    "$SCHEMA" "$event" "$hop" "$unit" "$st" "$reason" "$ec" "$kind" "$res" \
    "$ds_json" "$(humanise "$ds")" "$since_json" "$now_iso" "$rep" >> "$ALERTS"
}

hops_out=""
checked=0
down_count=0
unknown_count=0
alerts_emitted=0

for entry in $HOPS; do
  hop="${entry%%:*}"; rest="${entry#*:}"; scope="${rest%%:*}"; unit="${rest#*:}"
  if [ -z "$hop" ] || [ -z "$unit" ]; then continue; fi
  if [ -n "$FILTER" ]; then
    case " $FILTER " in *" $hop "*) ;; *) continue ;; esac
  fi
  checked=$(( checked + 1 ))

  classify "$scope" "$unit"
  status="$HOP_STATUS"; reason="$HOP_REASON"; exit_code="$HOP_EXIT"
  exit_kind="$HOP_EXIT_KIND"; result="$HOP_RESULT"; down_s="$HOP_DOWN_S"; since="$HOP_SINCE_ISO"
  prev="${PREV_STATUS[$hop]:-}"
  prev_alert="${PREV_ALERT[$hop]:-0}"
  if [ -z "$prev_alert" ] || [ "$prev_alert" = "" ]; then prev_alert=0; fi

  case "$status" in
    down|unknown)
      if [ "$status" = "down" ]; then down_count=$(( down_count + 1 )); else unknown_count=$(( unknown_count + 1 )); fi
      alert_this=0; repeat="false"
      if [ "$prev" != "$status" ]; then
        alert_this=1
      elif [ $(( now_epoch - prev_alert )) -ge "$REALERT" ]; then
        alert_this=1; repeat="true"
      fi
      if [ "$alert_this" = 1 ]; then
        alerts_emitted=$(( alerts_emitted + 1 ))
        prev_alert=$now_epoch
        append_alert "$hop" "down" "$unit" "$status" "$reason" "$exit_code" "$exit_kind" "$result" "$down_s" "$since" "$repeat"
        if [ "$JSON" = 0 ]; then
          if [ "$repeat" = "true" ]; then prefix="ALERT-REMINDER"; else prefix="ALERT"; fi
          printf '%s hop=%s unit=%s state=%s exit=%s kind=%s result=%s down=%s since=%s reason=%s\n' \
            "$prefix" "$hop" "$unit" "$status" "$exit_code" "$exit_kind" "$result" \
            "$( [ -n "$down_s" ] && humanise "$down_s" || printf 'unknown' )" \
            "${since:-unknown}" "$reason"
        fi
      fi ;;
    up)
      if [ "$prev" = "down" ] || [ "$prev" = "unknown" ]; then
        append_alert "$hop" "recovered" "$unit" "up" "HOP_UP" "$exit_code" "$exit_kind" "$result" "" "" "false"
        if [ "$JSON" = 0 ]; then
          printf 'RECOVERED hop=%s unit=%s was_down=%s exit=%s\n' \
            "$hop" "$unit" "$(humanise "${PREV_DOWN_S[$hop]:-}")" "$exit_code"
        fi
      fi
      if [ "$VERBOSE" = 1 ] && [ "$JSON" = 0 ]; then
        printf 'up hop=%s unit=%s state=%s\n' "$hop" "$unit" "$HOP_STATE_WORD"
      fi ;;
    transition)
      [ "$VERBOSE" = 1 ] && [ "$JSON" = 0 ] && printf 'transition hop=%s unit=%s state=%s\n' "$hop" "$unit" "$HOP_STATE_WORD" ;;
    *)
      : ;;
  esac

  hops_out+="${hop}"$'\t'"${status}"$'\t'"${prev_alert}"$'\t'"${down_s:-}"$'\n'
done

# Hops excluded by --hop keep the state they had; a filtered run must never
# erase the memory that makes an unfiltered run's next alert a transition.
seen=" "
while IFS=$'\t' read -r h _rest; do
  if [ -z "${h:-}" ] || [ "$h" = "hop" ]; then continue; fi
  seen+="${h} "
done <<< "$hops_out"
for h in "${!PREV_STATUS[@]}"; do
  case "$seen" in *" $h "*) continue ;; esac
  hops_out+="${h}"$'\t'"${PREV_STATUS[$h]}"$'\t'"${PREV_ALERT[$h]:-0}"$'\t'"${PREV_DOWN_S[$h]:-}"$'\n'
done

{
  printf 'hop\tstatus\tlast_alert_epoch\tdown_s\n'
  printf '%s' "$hops_out"
} > "${STATE}.tmp.$$" && mv -f "${STATE}.tmp.$$" "$STATE" || die "cannot write state file $STATE"

if [ "$JSON" = 1 ]; then
  obj=""
  while IFS=$'\t' read -r h st al ds; do
    if [ -z "${h:-}" ] || [ "$h" = "hop" ]; then continue; fi
    [ -n "$obj" ] && obj+=","
    if [ -n "${ds:-}" ]; then ds_json="$ds"; else ds_json="null"; fi
    obj+=$(printf '{"hop":"%s","status":"%s","down_for":"%s","down_s":%s}' "$h" "$st" "$(humanise "${ds:-}")" "$ds_json")
  done <<< "$hops_out"
  printf '{"schema":"%s","at":"%s","checked":%d,"up":%d,"down":%d,"unknown":%d,"alerts_emitted":%d,"hops":[%s]}\n' \
    "$SCHEMA" "$now_iso" "$checked" "$(( checked - down_count - unknown_count ))" \
    "$down_count" "$unknown_count" "$alerts_emitted" "$obj"
fi

if [ "$STRICT" = 1 ] && [ $(( down_count + unknown_count )) -gt 0 ]; then
  exit 2
fi
exit 0
