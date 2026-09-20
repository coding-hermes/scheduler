#!/usr/bin/env bash
# SCHED-GAP-147 — the live-config invariant fixture battery.
#
# One command that runs EVERYTHING the invariant checker owes its regression
# gate:
#   1. the checker itself in --board-only mode (the CI shape: environment-
#      independent, exit 1 on any real board violation in the checkout);
#   2. the full pytest fixture battery (every violation class, both arms —
#      seeded violation exits 1, conforming fleet exits 0), self-contained
#      by construction: temp DBs, temp TOMLs, no ~/.hermes state.
#
# Exit non-zero if ANY part fails. Run it after every scheduler deploy and
# from the daily report; CI runs it in the build job (a GitHub runner has no
# ~/.hermes/coding-hermes/scheduler.db — that is why the battery, not the
# live-fleet checks, is what runs there).
#
# Usage:            scripts/check-invariants.sh
# From anywhere:    bash /path/to/repo/scripts/check-invariants.sh
set -u
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

rc_board=0
rc_pytest=0

echo "=== [1/2] check-fleet-invariants --board-only (CI shape) ==="
python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only
rc_board=$?
if [ "$rc_board" -ne 0 ]; then
    echo "FAIL: board-only invariant check exited $rc_board" >&2
fi

echo
echo "=== [2/2] fixture battery (pytest tests/) ==="
python3 -m pytest tests/ -q
rc_pytest=$?
if [ "$rc_pytest" -ne 0 ]; then
    echo "FAIL: fixture battery exited $rc_pytest" >&2
fi

if [ "$rc_board" -ne 0 ] || [ "$rc_pytest" -ne 0 ]; then
    echo
    echo "BATTERY: FAIL (board=$rc_board pytest=$rc_pytest)"
    exit 1
fi
echo
echo "BATTERY: PASS (board-only clean, fixture battery green)"
exit 0
