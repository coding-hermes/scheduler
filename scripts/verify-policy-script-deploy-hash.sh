#!/usr/bin/env bash
#
# verify-policy-script-deploy-hash.sh — SCHED-PERF-006 mirror of the
# "verify" step in ~/.hermes/scripts/scheduler-deploy-drain-restart.sh.
#
# WHY THIS EXISTS
# ---------------
# The restart script's verify block (caps, admission, migration, fix
# presence) never checks the policy script's integrity, so a restart on a
# corrupted ~/.hermes/scripts/fleet-cooldown-policy.py proceeds without
# protest — and scripts/sync_runtime.sh (task-router) will then deploy the
# repo copy over the operator's live fix on its next run (the
# "non-canonical live copy replaced" clobber).
#
# The in-tree guard scripts/policy-script-deploy-hash-guard.sh decides
# clean/corrupt (exit 0 / non-zero; see its header for the code table).
# This wrapper is the restart-shaped surface: same invocation the
# deploy-drain-restart "verify" block calls, log lines in the restart
# script's "   " style, ABORT (non-zero) on any guard refusal. ABORT
# semantics downstream: do NOT restart the systemd unit, do NOT swap the
# binary, do NOT silently continue.
#
# INSTALL (the foreman's one-line drop-in for the restart script's verify
# block, after the caps/admission check and before the FIX PRESENCE check):
#
#   if [ -x /home/kara/coding-hermes-scheduler/coding-herms-scheduler/scripts/verify-policy-script-deploy-hash.sh ]; then
#     /home/kara/.../scripts/verify-policy-script-deploy-hash.sh || exit 4
#   fi
#
# EXIT CODES
#   0  policy script verified clean (hash matches the canonical sidecar)
#   4  ABORT — deploy-hash mismatch / missing / unreadable (propagated and
#      logged); the caller must abort the deploy
#
# RED/GREEN proof lives with the guard: tests/test_policy_script_deploy_hash_guard.sh

set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GUARD="$REPO_ROOT/scripts/policy-script-deploy-hash-guard.sh"

if [ ! -x "$GUARD" ]; then
    echo "   !! VERIFY ABORT: guard not found or not executable at $GUARD"
    exit 4
fi

OUT="$("$GUARD" 2>&1)"
RC=$?

if [ "$RC" -eq 0 ]; then
    echo "$OUT" | sed 's/^/   /'
    exit 0
fi

echo "$OUT" | sed 's/^/   /'
echo "   !! VERIFY ABORT: fleet-cooldown-policy.py deploy-hash check FAILED (guard rc=$RC)"
echo "   !! refusing to continue the deploy — a consumer deploy/restart now would CLOBBER the live fix"
exit 4
