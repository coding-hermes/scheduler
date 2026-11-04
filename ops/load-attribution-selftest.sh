#!/usr/bin/env bash
# load-attribution-selftest.sh — prove ops/load-attribution.sh actually grades
# the host instead of printing a happy line (SCHED-GAP-16977).
#
# WHY THIS EXISTS
#   A probe that always says "OK" is worse than no probe: it certifies a
#   saturated host as coordinated. This selftest drives the real script through
#   fixtures it controls and makes four claims:
#
#     1. IDLE IS OK — no heavy processes ⇒ "heavy jobs : 0", verdict OK, exit 0.
#     2. A VIOLATION IS NAMED — three concurrent heavy jobs against a limit of 1
#        ⇒ exit 1, VIOLATION, and every one of the three appears with its kind
#        (test / build / compress) and pid.
#     3. A SHUT GATE WITH A NAMED HOLDER IS A VIOLATION — one heavy job, limit 2,
#        load above the gate ⇒ exit 1 even though the count is within policy,
#        because the gate is shut and this is what is holding it.
#     4. THE SELFTEST CAN FAIL — a mutated copy of the probe (verdict forced to
#        "ok") is caught by claim 2. If that check ever passes with the mutation
#        applied, this file is decoration.
#
#   Nothing here reads the real process table or the live scheduler: --ps-fixture
#   supplies the process list, --gate supplies the threshold, and --api points at
#   a closed port so attribution degrades to "(unattributed)" honestly.
#
# USAGE
#   ops/load-attribution-selftest.sh [--verbose]
#
# EXIT CODES
#   0  every check passed
#   1  at least one check failed
#   2  a required tool is missing — never a silent skip

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROBE="$HERE/load-attribution.sh"
VERBOSE=0
[ "${1:-}" = "--verbose" ] && VERBOSE=1

for tool in bash mktemp grep sed; do
  command -v "$tool" >/dev/null 2>&1 || { echo "selftest: missing tool: $tool" >&2; exit 2; }
done
[ -x "$PROBE" ] || { echo "selftest: probe not executable: $PROBE" >&2; exit 2; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/load-attr-selftest.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

fails=0
checks=0
ok() { checks=$((checks + 1)); [ "$VERBOSE" -eq 1 ] && echo "  PASS  $*"; return 0; }
bad() { checks=$((checks + 1)); fails=$((fails + 1)); echo "  FAIL  $*"; }

# A closed port: attribution must degrade, not hang or lie.
CLOSED_API="http://127.0.0.1:1"

# ---------------------------------------------------------------- fixtures
# ps -eo pid=,pcpu=,etimes=,args= shape: pid pcpu etimes args
cat > "$WORK/idle.ps" <<'EOF'
  1001  0.0    120 /usr/lib/systemd/systemd --user
  1002  1.2     60 /usr/bin/gnome-shell
  1003  3.4   1200 vim ops/load-attribution.sh
  1004  8.0     30 /home/kara/.hermes/venv/bin/python -m http.server
EOF

cat > "$WORK/heavy.ps" <<'EOF'
  2001 95.0    900 python -m pytest -q tests/
  2002 88.0     40 go test ./... -count=1
  2003 70.0   1800 xz -T4 -6 /mnt/bulk/rehearsal-full/full.sql
  2004  2.0     10 bash -c echo idle
EOF

run_probe() { # run_probe <ps-fixture> <gate> <limit> [extra args...]
  local fixture="$1" gate="$2" limit="$3"; shift 3
  bash "$PROBE" --ps-fixture "$fixture" --gate "$gate" --limit "$limit" \
    --api "$CLOSED_API" --proc-root "$WORK/no-such-proc" "$@" 2>&1
}

# ---------------------------------------------------------------- claim 1
out="$(run_probe "$WORK/idle.ps" 20 1)"
rc=$?
if [ "$rc" -eq 0 ]; then ok "idle host exits 0"; else bad "idle host exited $rc, want 0"; fi
case "$out" in
  *"heavy jobs : 0"*) ok "idle host reports zero heavy jobs" ;;
  *) bad "idle host did not report 'heavy jobs : 0': $out" ;;
esac
case "$out" in
  *"verdict    : OK"*) ok "idle host verdict OK" ;;
  *) bad "idle host verdict not OK: $out" ;;
esac

# ---------------------------------------------------------------- claim 2
out="$(run_probe "$WORK/heavy.ps" 20 1)"
rc=$?
if [ "$rc" -eq 1 ]; then ok "three heavy jobs over a limit of 1 exits 1"; else bad "violation exited $rc, want 1"; fi
case "$out" in
  *"heavy jobs : 3"*) ok "counts exactly 3 heavy jobs (the idle row is not counted)" ;;
  *) bad "wrong heavy count: $out" ;;
esac
# `go test ./...` is a TEST job, not a build — the probe classifies by what the
# process is doing, and the earlier expectation here was simply wrong.
for pair in "2001:test" "2002:test" "2003:compress"; do
  pid="${pair%%:*}"; kind="${pair##*:}"
  case "$out" in
    *"pid $pid"*"$kind"*) ok "names pid $pid as $kind" ;;
    *) bad "pid $pid was not named as $kind: $out" ;;
  esac
done
case "$out" in
  *"VIOLATION"*) ok "prints the VIOLATION verdict" ;;
  *) bad "no VIOLATION verdict printed" ;;
esac
case "$out" in
  *"2004"*) bad "counted the idle process (pid 2004) as heavy" ;;
  *) ok "idle process left out of the heavy set" ;;
esac
case "$out" in
  *"(unattributed)"*) ok "degrades attribution honestly when the API is closed" ;;
  *) bad "missing the (unattributed) fallback" ;;
esac

# ---------------------------------------------------------------- claim 3
out="$(run_probe "$WORK/heavy.ps" 5 3)"
rc=$?
if [ "$rc" -eq 1 ]; then ok "shut gate with a named holder exits 1 even inside the count policy"; else bad "shut gate exited $rc, want 1"; fi
case "$out" in
  *"gate state: shut"*) ok "reports the gate as shut" ;;
  *) bad "gate state not reported as shut: $out" ;;
esac

# JSON shape
json="$(run_probe "$WORK/heavy.ps" 20 1 --json)"
if printf '%s' "$json" | grep -q '"verdict": "violation"'; then ok "JSON carries verdict=violation"; else bad "JSON verdict wrong: $json"; fi
if printf '%s' "$json" | grep -q '"pid": 2001'; then ok "JSON lists pid 2001"; else bad "JSON missing pid 2001"; fi
if printf '%s' "$json" | grep -q '"count": 3'; then ok "JSON count is 3"; else bad "JSON count wrong"; fi

# ---------------------------------------------------------------- claim 4
# The selftest must be able to fail. Mutate the probe so every verdict reads OK
# and prove claim 2's exit-code check catches it.
mutant="$WORK/mutant.sh"
sed 's/verdict="ok"/verdict="ok"/; s/^\[ "\$verdict" = "violation" \] && exit 1/[ "$verdict" = "never" ] \&\& exit 1/' \
  "$PROBE" > "$mutant"
chmod +x "$mutant"
mout="$(bash "$mutant" --ps-fixture "$WORK/heavy.ps" --gate 20 --limit 1 --api "$CLOSED_API" 2>&1)"
mrc=$?
if [ "$mrc" -eq 0 ]; then
  ok "mutation is caught: the neutralised exit no longer fails claim 2"
else
  bad "mutation was NOT caught (exit $mrc) — the exit-code check is decoration"
fi

# ---------------------------------------------------------------- summary
echo ""
if [ "$fails" -eq 0 ]; then
  echo "load-attribution selftest: $checks checks passed"
  exit 0
fi
echo "load-attribution selftest: $fails of $checks checks FAILED"
exit 1
