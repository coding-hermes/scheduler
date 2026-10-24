#!/usr/bin/env bash
# SCHED-GAP-16926 — the post-deploy invariant battery.
#
# The battery has three stages:
#   1. the checker against the resolvable live scheduler DB + fleet.toml;
#      when that state is absent, the explicit board-only CI arm runs instead;
#   2. the lint-guard regression battery;
#   3. the full pytest fixture battery (temp DBs, TOMLs, and boards; no
#      ~/.hermes state required).
#
# Exit non-zero if ANY executed stage fails. On an operator host with live
# state, the final summary is a live-fleet verdict. On CI or another checkout
# without live state, the summary explicitly says that only board checks ran.
# Run it after every scheduler deploy and from the daily report.
#
# Usage:            scripts/check-invariants.sh
# From anywhere:    bash /path/to/repo/scripts/check-invariants.sh
set -u
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

STAGES=(
    "check-fleet-invariants"
    "lint-guard.sh regression battery (SCHED-GAP-221)"
    "fixture battery (pytest tests/)"
)
STAGE_COUNT=${#STAGES[@]}
STAGE_NO=0

stage_header() {
    STAGE_NO=$((STAGE_NO + 1))
    printf '=== [%d/%d] %s ===\n' "$STAGE_NO" "$STAGE_COUNT" "$1"
}

rc_checker=0
rc_lint=0
rc_pytest=0
live_mode=0
live_violations=""

live_db="${HOME:-}/.hermes/coding-hermes/scheduler.db"
live_toml="${HOME:-}/.hermes/fleet.toml"
if [ -r "$live_db" ] && [ -r "$live_toml" ]; then
    live_mode=1
fi

if [ "$live_mode" -eq 1 ]; then
    stage_header "${STAGES[0]} (live DB + TOML)"
    checker_output="$(mktemp "${TMPDIR:-/tmp}/check-invariants.XXXXXX")"
    python3 ops/check-fleet-invariants.py \
        --db "$live_db" \
        --toml "$live_toml" \
        --board .coding-hermes/board/tasks.jsonl \
        >"$checker_output" 2>&1
    rc_checker=$?
    cat "$checker_output"
    live_violations="$(sed -nE 's/.*— ([0-9]+) violation\(s\).*/\1/p' "$checker_output" | tail -n 1)"
    rm -f "$checker_output"
    if [ -z "$live_violations" ]; then
        live_violations="unknown"
    fi
    if [ "$rc_checker" -ne 0 ]; then
        echo "FAIL: live-fleet invariant check exited $rc_checker" >&2
    fi
else
    stage_header "${STAGES[0]} --board-only (no live DB/TOML; CI arm)"
    python3 ops/check-fleet-invariants.py \
        --board .coding-hermes/board/tasks.jsonl \
        --board-only
    rc_checker=$?
    if [ "$rc_checker" -ne 0 ]; then
        echo "FAIL: board-only invariant check exited $rc_checker" >&2
    fi
fi

echo
stage_header "${STAGES[1]}"
bash tests/test_lint_guard.sh
rc_lint=$?
if [ "$rc_lint" -ne 0 ]; then
    echo "FAIL: lint-guard battery exited $rc_lint" >&2
fi

echo
stage_header "${STAGES[2]}"
python3 -m pytest tests/ -q
rc_pytest=$?
if [ "$rc_pytest" -ne 0 ]; then
    echo "FAIL: fixture battery exited $rc_pytest" >&2
fi

echo
if [ "$live_mode" -eq 1 ]; then
    if [ "$rc_checker" -ne 0 ] || [ "$rc_lint" -ne 0 ] || [ "$rc_pytest" -ne 0 ]; then
        echo "BATTERY: FAIL (live fleet checked; live violations=$live_violations; checker=$rc_checker lint=$rc_lint pytest=$rc_pytest)"
        exit 1
    fi
    echo "BATTERY: PASS (live fleet checked; live violations=0; lint-guard green, fixture battery green)"
else
    if [ "$rc_checker" -ne 0 ] || [ "$rc_lint" -ne 0 ] || [ "$rc_pytest" -ne 0 ]; then
        echo "BATTERY: FAIL (board-only; live fleet NOT checked; fleet checks not run: no local DB/TOML; checker=$rc_checker lint=$rc_lint pytest=$rc_pytest)"
        exit 1
    fi
    echo "BATTERY: PASS (board-only; live fleet NOT checked; fleet checks not run: no local DB/TOML; lint-guard green, fixture battery green)"
fi
exit 0
