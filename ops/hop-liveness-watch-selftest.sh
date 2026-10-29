#!/usr/bin/env bash
#
# hop-liveness-watch-selftest.sh — prove that ops/hop-liveness-watch.sh CAN
# alert, that it stays silent when it should, and that its alert names the hop,
# the exit status and how long the hop has been down.
#
# WHY THIS EXISTS
#   A watchdog that never fires is indistinguishable from a healthy fleet. This
#   selftest drives the real watcher against a stub `systemctl` whose answers it
#   controls, and makes five claims, each with its own checks:
#
#     1. SILENT WHEN CLEAN — four units active ⇒ no stdout, exit 0, and an
#        alerts file that was never even created.
#     2. A CLEAN EXIT IS A FINDING — a unit that is inactive with
#        Result=success / ExecMainStatus=0 (exactly crier-fleet on 2026-10-28)
#        produces one loud line naming hop, unit, exit status and down time
#        ("41m" from a purpose-built monotonic stamp), plus one JSON record with
#        reason=HOP_DOWN_CLEAN_EXIT.
#     3. TRANSITIONS, NOT POLLS — a second run over the same state emits
#        nothing; with --re-alert 0 it emits a reminder flagged repeat=true; a
#        unit that comes back writes a `recovered` record.
#     4. REFUSALS ARE NAMED — a failed unit reports reason=HOP_DOWN_FAILURE with
#        its real exit code; a unit that does not exist reports
#        reason=HOP_UNIT_NOT_FOUND instead of silence, and --strict exits 2 for
#        both.
#     5. THE SELFTEST CAN FAIL — a mutated copy of the watcher (the `active`
#        branch reclassified as down) is caught by claim 1. If this check ever
#        passes while the mutation is applied, the selftest is decoration.
#
#   Nothing here touches the live fleet: the stub replaces `systemctl` on PATH
#   and every path (state, alerts, fixtures) lives under a mktemp directory.
#
# USAGE
#   ops/hop-liveness-watch-selftest.sh [--verbose]
#
# EXIT CODES
#   0  every check passed
#   1  at least one check failed (the watcher is not proving what it claims)
#   2  a required tool is missing (bash / date / awk / sed) — never a silent skip

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WATCH="$HERE/hop-liveness-watch.sh"
VERBOSE=0
[ "${1-}" = "--verbose" ] && VERBOSE=1

TMP="$(mktemp -d "${TMPDIR:-/tmp}/hop-liveness-selftest.XXXXXX")" || exit 2
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
hdr()  { printf '\n%s\n' "== $*"; }
pass() { printf '  PASS  %s\n' "$1"; PASS=$(( PASS + 1 )); }
fail() { printf '  FAIL  %s\n' "$1"; FAIL=$(( FAIL + 1 )); }
note() { [ "$VERBOSE" = 1 ] && printf '        %s\n' "$1"; }

expect_eq()      { if [ "$2" = "$3" ]; then pass "$1 ($3)"; else fail "$1: expected '$3', got '$2'"; fi; }
expect_rc()      { if [ "$2" = "$3" ]; then pass "$1 (rc=$3)"; else fail "$1: expected rc $3, got $2"; fi; }
expect_has()     { if printf '%s' "$2" | grep -qF -- "$3"; then pass "$1"; else fail "$1: '$3' not found in: $2"; fi; }
expect_absent()  { if printf '%s' "$2" | grep -qF -- "$3"; then fail "$1: '$3' unexpectedly present in: $2"; else pass "$1"; fi; }

for t in bash date awk sed grep mktemp; do
  command -v "$t" >/dev/null 2>&1 || { printf 'missing required tool: %s\n' "$t" >&2; exit 2; }
done

# ---- stub systemctl ---------------------------------------------------------
mkdir -p "$TMP/bin" "$TMP/fix" "$TMP/out"
cat > "$TMP/bin/systemctl" <<'STUB'
#!/usr/bin/env bash
# Stub: prints the properties for the requested unit from $STUB_FIX/<unit>.props
# (or a LoadState=not-found record), ignoring -p selection the way the real
# tool does when asked for a subset it has.
unit=""
skip=0
for a in "$@"; do
  if [ "$skip" = 1 ]; then skip=0; continue; fi
  case "$a" in
    show|--user|--system) ;;
    -p|--property) skip=1 ;;
    -*) ;;
    *) [ -z "$unit" ] && unit="$a" ;;
  esac
done
printf '%s\n' "$*" >> "${STUB_LOG:-/dev/null}"
if [ -r "${STUB_FIX}/${unit}.props" ]; then
  cat "${STUB_FIX}/${unit}.props"
else
  printf 'LoadState=not-found\nActiveState=inactive\nSubState=dead\nResult=success\nExecMainCode=0\nExecMainStatus=0\nStateChangeTimestampMonotonic=n/a\nStateChangeTimestamp=n/a\n'
fi
STUB
chmod +x "$TMP/bin/systemctl"

export PATH="$TMP/bin:$PATH"
export STUB_FIX="$TMP/fix"
export STUB_LOG="$TMP/systemctl-calls.log"

UPTIME="$(awk '{printf "%d", $1}' /proc/uptime)"

# props_up <unit>
props_up() {
  cat > "$TMP/fix/$1.props" <<EOF
LoadState=loaded
ActiveState=active
SubState=running
Result=success
ExecMainCode=0
ExecMainStatus=0
StateChangeTimestampMonotonic=$(( UPTIME * 1000000 ))
StateChangeTimestamp=$(date -d "@$(( $(date -u +%s) - UPTIME ))" '+%a %Y-%m-%d %H:%M:%S %Z')
EOF
}
# props_down <unit> <exit_status> <result> <down_seconds>
props_down() {
  local mono=$(( (UPTIME - $4) * 1000000 ))
  [ "$mono" -lt 0 ] && mono=0
  cat > "$TMP/fix/$1.props" <<EOF
LoadState=loaded
ActiveState=inactive
SubState=dead
Result=$3
ExecMainCode=1
ExecMainStatus=$2
StateChangeTimestampMonotonic=$mono
StateChangeTimestamp=$(date -d "@$(( $(date -u +%s) - $4 ))" '+%a %Y-%m-%d %H:%M:%S %Z')
EOF
}
props_failed() {
  cat > "$TMP/fix/$1.props" <<EOF
LoadState=loaded
ActiveState=failed
SubState=failed
Result=exit-code
ExecMainCode=1
ExecMainStatus=$2
StateChangeTimestampMonotonic=$(( (UPTIME - $3) * 1000000 ))
StateChangeTimestamp=$(date -d "@$(( $(date -u +%s) - $3 ))" '+%a %Y-%m-%d %H:%M:%S %Z')
EOF
}

run() { # run <tag> [args...] -> writes $TMP/out/<tag>.out, sets RC
  local tag="$1"; shift
  "$WATCH" --state "$TMP/state.tsv" --alerts "$TMP/alerts.jsonl" "$@" > "$TMP/out/$tag.out" 2> "$TMP/out/$tag.err"
  RC=$?
  note "run $tag rc=$RC out=$(tr '\n' '|' < "$TMP/out/$tag.out")"
}

reset() { rm -f "$TMP/state.tsv" "$TMP/state.tsv.lock" "$TMP/alerts.jsonl"; rm -f "$TMP/fix"/*.props; }

# ============================================================================
hdr "1. silent when clean"
reset
for u in task-router-proxy.service crier-fleet.service coding-hermes-scheduler.service bunkerd.service; do props_up "$u"; done
run clean
expect_rc "four active units exit 0" "$RC" 0
expect_eq "no output when every hop is up" "$(cat "$TMP/out/clean.out")" ""
if [ -e "$TMP/alerts.jsonl" ]; then fail "an alerts file was created while every hop was up"; else pass "no alerts file created while every hop was up"; fi

# ============================================================================
hdr "2. a clean exit is a finding (the crier-fleet 11h outage shape)"
reset
for u in task-router-proxy.service coding-hermes-scheduler.service bunkerd.service; do props_up "$u"; done
props_down crier-fleet.service 0 success 2460     # inactive, clean exit, 41 minutes ago
run cleanexit
expect_rc "a down hop is a finding, not a watcher fault: exit 0" "$RC" 0
OUT="$(cat "$TMP/out/cleanexit.out")"
expect_has "alert names the hop"            "$OUT" "hop=crier"
expect_has "alert names the unit"           "$OUT" "unit=crier-fleet.service"
expect_has "alert names the exit status"    "$OUT" "exit=0"
expect_has "alert names how it ended"       "$OUT" "kind=exited"
expect_has "alert names the down duration"  "$OUT" "down=41m"
expect_has "alert names the reason code"    "$OUT" "reason=HOP_DOWN_CLEAN_EXIT"
expect_eq  "exactly one alert line"         "$(grep -c '^ALERT hop=' "$TMP/out/cleanexit.out")" "1"
REC="$(head -1 "$TMP/alerts.jsonl")"
expect_has "record carries the reason code"  "$REC" '"reason":"HOP_DOWN_CLEAN_EXIT"'
expect_has "record carries event=down"       "$REC" '"event":"down"'
expect_has "record carries a numeric down_s" "$REC" '"down_s":24'
expect_has "record carries the ISO since"    "$REC" '"since":"'
expect_has "record is attributed to the watcher" "$REC" '"watchdog":"ops/hop-liveness-watch.sh"'

# ============================================================================
hdr "3. transitions, not polls"
run same-again
expect_eq "a second run over the same state is silent" "$(cat "$TMP/out/same-again.out")" ""
expect_eq "and appends nothing"                        "$(wc -l < "$TMP/alerts.jsonl" | tr -d ' ')" "1"
run remind --re-alert 0
expect_has "a re-alert names the hop again"   "$(cat "$TMP/out/remind.out")" "ALERT-REMINDER hop=crier"
expect_has "the reminder is flagged repeat"   "$(tail -1 "$TMP/alerts.jsonl")" '"repeat":true'
props_up crier-fleet.service
run recovered
expect_has "a hop that answers again is reported" "$(cat "$TMP/out/recovered.out")" "RECOVERED hop=crier"
expect_has "recovery records how long it was down" "$(tail -1 "$TMP/alerts.jsonl")" '"event":"recovered"'
expect_eq  "recovery does not repeat on the next clean run" \
  "$( run after-recovery; cat "$TMP/out/after-recovery.out" )" ""

# ============================================================================
hdr "4. refusals are named"
reset
for u in task-router-proxy.service crier-fleet.service coding-hermes-scheduler.service; do props_up "$u"; done
props_failed bunkerd.service 1 300
run failed
expect_has "a failed unit reports the failure reason" "$(cat "$TMP/out/failed.out")" "reason=HOP_DOWN_FAILURE"
expect_has "and the real exit code"                   "$(cat "$TMP/out/failed.out")" "exit=1"
expect_has "and how long it has been down"            "$(cat "$TMP/out/failed.out")" "down=5m"
run failed-strict --strict
expect_rc  "--strict exits 2 for a down hop" "$RC" 2

reset
for u in task-router-proxy.service crier-fleet.service coding-hermes-scheduler.service bunkerd.service; do props_up "$u"; done
rm -f "$TMP/fix/crier-fleet.service.props"        # stub answers LoadState=not-found
run missing
expect_has "a unit that does not exist is a named refusal, not silence" \
  "$(cat "$TMP/out/missing.out")" "reason=HOP_UNIT_NOT_FOUND"
run missing-strict --strict
expect_rc  "--strict exits 2 for an unknown hop" "$RC" 2
expect_has "the JSON summary counts the unknown hop" \
  "$( "$WATCH" --state "$TMP/state.tsv" --alerts "$TMP/alerts.jsonl" --json )" '"unknown":1'

# ============================================================================
hdr "5. the selftest can fail (mutation control)"
MUT="$TMP/mutated-watch.sh"
sed 's/      HOP_STATUS="up"; HOP_REASON="HOP_UP"; return 0 ;;/      HOP_STATUS="down"; HOP_REASON="HOP_UP"; return 0 ;;/' \
  "$WATCH" > "$MUT"
chmod +x "$MUT"
if cmp -s "$WATCH" "$MUT"; then
  fail "mutation did not apply — the mutation point moved, update this selftest"
else
  pass "mutation applied at the active-state branch"
  reset
  for u in task-router-proxy.service crier-fleet.service coding-hermes-scheduler.service bunkerd.service; do props_up "$u"; done
  "$MUT" --state "$TMP/state.tsv" --alerts "$TMP/alerts.jsonl" > "$TMP/out/mutated.out" 2>/dev/null
  MRC=$?
  if grep -q '^ALERT hop=' "$TMP/out/mutated.out"; then
    pass "the mutated watcher alerts on a healthy fleet — claim 1 would catch it (rc=$MRC)"
  else
    fail "the mutated watcher stayed silent: the selftest cannot distinguish up from down"
  fi
fi

# ============================================================================
printf '\n%s\n' "== summary"
printf '   %d checks: %d passed, %d failed\n' "$(( PASS + FAIL ))" "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then exit 1; fi
exit 0
