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
# STREAM CONTRACT (proven by tests/test_verify_policy_script_deploy_hash.sh)
#   stdout: the guard's informational output on a clean run
#   stderr: every ABORT line ("VERIFY ABORT", the guard's "[SCHED-PERF-006]"
#           marker, the clobber warning) — a verify block that logs the two
#           streams differently must not see abort noise on stdout
#
# RED/GREEN proof:
#   guard   — tests/test_policy_script_deploy_hash_guard.sh
#   wrapper — tests/test_verify_policy_script_deploy_hash.sh
#
# DEPLOY FRAGMENT (operator-applied, not automatic)
# -------------------------------------------------
# The exact snippet the operator pastes into
# ~/.hermes/scripts/scheduler-deploy-drain-restart.sh's verify block
# (after the caps/admission check, before the FIX PRESENCE check). Nothing
# in this repo or CI applies it to the live operator file. Self-contained:
# survives this wrapper being absent (test -x guard) and maps any refusal
# (or a missing wrapper) to exit 4 so the restart aborts.
#
#   # SCHED-PERF-006: policy-script deploy-hash verify (operator-applied)
#   WRAP=/home/kara/coding-hermes-scheduler/coding-herms-scheduler/scripts/verify-policy-script-deploy-hash.sh
#   if [ -x "$WRAP" ]; then
#       "$WRAP" || exit 4
#   else
#       echo "   !! VERIFY ABORT: $WRAP not found/deployed" >&2
#       echo "   !! refusing to continue the deploy — cannot verify fleet-cooldown-policy.py integrity" >&2
#       exit 4
#   fi

set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GUARD="$REPO_ROOT/scripts/policy-script-deploy-hash-guard.sh"

if [ ! -x "$GUARD" ]; then
    echo "   !! VERIFY ABORT: guard not found or not executable at $GUARD" >&2
    exit 4
fi

OUT="$("$GUARD" 2>&1)"
RC=$?

if [ "$RC" -eq 0 ]; then
    echo "$OUT" | sed 's/^/   /'
    exit 0
fi

# ABORT path: everything goes to STDERR. The guard's captured output already
# carries its own [SCHED-PERF-006] marker; re-emit it on this stream so the
# restart log's error surface sees the reason, not just the verdict.
echo "$OUT" | sed 's/^/   /' >&2
echo "   !! VERIFY ABORT: fleet-cooldown-policy.py deploy-hash check FAILED (guard rc=$RC)" >&2
echo "   !! refusing to continue the deploy — a consumer deploy/restart now would CLOBBER the live fix" >&2
exit 4
