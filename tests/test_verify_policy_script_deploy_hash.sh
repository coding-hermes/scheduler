#!/usr/bin/env bash
#
# test_verify_policy_script_deploy_hash.sh — restart-shape integration test
# for scripts/verify-policy-script-deploy-hash.sh (SCHED-PERF-006).
#
# WHY: the wrapper had no caller in the test suite — the deploy-drain-restart
# wiring was documented in its header but never PROVEN. This test drives the
# wrapper exactly the way the restart script's verify block would drain it:
# wrapper invoked with the guard's site-override pointing at a hermetic
# fixture "live" (clean, then corrupted), asserting the wrapper's exit-code
# contract AND its stream discipline:
#
#   clean fixture    -> exit 0, "deploy hash OK" text, abort markers OFF stdout
#   corrupted fixture-> exit 4 (documented ABORT), "VERIFY ABORT" AND the
#                       guard's "[SCHED-PERF-006]" marker on STDERR (not
#                       stdout — a verify block must be able to log the two
#                       streams differently)
#   wrapper w/o guard-> exit 4, "guard not found" on stderr (fail-closed)
#
# Shape: mirrors tests/test_policy_script_deploy_hash_guard.sh — hermetic
# temp dir with its own fixture copies, no writes to the real live fleet
# state (the real live file and sidecar are only ever READ to bootstrap the
# fixture).
#
# Note on the exit contract: this test itself exits 0 when BOTH wrapper
# branches (OK and ABORT) behave as documented — an ABORT inside a fixture
# is the expected outcome, not a failure of the test.
#
# Requires: bash, python3, coreutils.

set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WRAPPER="$REPO_ROOT/scripts/verify-policy-script-deploy-hash.sh"
GUARD="$REPO_ROOT/scripts/policy-script-deploy-hash-guard.sh"
if [ ! -f "$WRAPPER" ]; then
    echo "FAIL: wrapper not found at $WRAPPER" >&2
    exit 1
fi
if [ ! -f "$GUARD" ]; then
    echo "FAIL: guard not found at $GUARD (the wrapper calls it)" >&2
    exit 1
fi

REAL_LIVE="$HOME/.hermes/scripts/fleet-cooldown-policy.py"
REAL_SIDECAR="$HOME/.hermes/scripts/.fleet-cooldown-policy.canonical.sha256"

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

DEPLOY="$TMP_ROOT/deploy"
STDOUT="$TMP_ROOT/wrapper_stdout.txt"
STDERR="$TMP_ROOT/wrapper_stderr.txt"
mkdir -p "$DEPLOY"

if [ ! -f "$REAL_LIVE" ]; then
    echo "SKIP: live fleet-cooldown-policy.py not found at $REAL_LIVE (nothing to fixture)" >&2
    exit 0
fi
cp "$REAL_LIVE" "$DEPLOY/fleet-cooldown-policy.py"

if [ -f "$REAL_SIDECAR" ]; then
    cp "$REAL_SIDECAR" "$DEPLOY/.fleet-cooldown-policy.canonical.sha256"
else
    # Bootstrap the fixture sidecar from the canonical live hash: same
    # semantics the real script performs on its first --apply.
    printf '%s\n' "$(sha256sum "$REAL_LIVE" | cut -d' ' -f1)" \
        > "$DEPLOY/.fleet-cooldown-policy.canonical.sha256"
fi

# Restart-shaped invocation: the verify block in the deploy-drain-restart
# script calls the wrapper and captures both streams. The guard's
# POLICY_GUARD_SCRIPT site-override propagates through the wrapper to the
# guard unchanged, so the fixture IS the "live" for the whole chain.
run_wrapper() {
    POLICY_GUARD_SCRIPT="$DEPLOY/fleet-cooldown-policy.py" \
        bash "$WRAPPER" >"$STDOUT" 2>"$STDERR"
}

# ──────────────────────────────────────────────────────────────────────────
# Case 1 (OK): clean fixture → wrapper exits 0, hash-OK text on stdout,
# no ABORT markers anywhere.
# ──────────────────────────────────────────────────────────────────────────
run_wrapper
RC=$?
echo "WRAP-OK case (clean fixture): exit=$RC"
if [ "$RC" -ne 0 ]; then
    echo "FAIL: wrapper should exit 0 on a clean fixture, got $RC"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if ! grep -q 'deploy hash OK' "$STDOUT"; then
    echo "FAIL: expected 'deploy hash OK' on the wrapper's stdout"
    echo "--- stdout ---"; cat "$STDOUT"
    exit 1
fi
if grep -q 'VERIFY ABORT' "$STDOUT" "$STDERR"; then
    echo "FAIL: ABORT markers must not appear on a clean run"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────────
# Case 2 (ABORT): corrupted fixture → wrapper exits 4 (its documented ABORT),
# with the "VERIFY ABORT" marker AND the guard's "[SCHED-PERF-006]" marker on
# STDERR — and ,(deploy-restart streams are separated) NOT both on stdout.
# Mutates the FIXTURE only; the real live file is never touched.
# ──────────────────────────────────────────────────────────────────────────
printf '\nprint("STALE_VARIANT_%s")\n' "$(date +%s)" \
    >> "$DEPLOY/fleet-cooldown-policy.py"
run_wrapper
RC=$?
echo "WRAP-ABORT case (mutated fixture): exit=$RC"
if [ "$RC" -eq 0 ]; then
    echo "FAIL: wrapper must NOT exit 0 on a corrupted live (that is the clobber)"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if [ "$RC" -ne 4 ]; then
    echo "FAIL: wrapper should exit 4 (documented ABORT) on a corrupted live, got $RC"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if ! grep -q 'VERIFY ABORT' "$STDERR"; then
    echo "FAIL: expected the 'VERIFY ABORT' marker on the wrapper's STDERR"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if ! grep -qF '[SCHED-PERF-006]' "$STDERR"; then
    echo "FAIL: expected the guard's [SCHED-PERF-006] marker propagated onto STDERR"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if grep -q 'VERIFY ABORT' "$STDOUT"; then
    echo "FAIL: 'VERIFY ABORT' leaked onto stdout (verify block cannot stream-split)"
    echo "--- stdout ---"; cat "$STDOUT"
    exit 1
fi

# Side effect check: the ABORT must not launder the corruption — the fixture
# sidecar still holds the OLD canonical hash.
CANON_BEFORE="$(tr -d ' \t\n' < "$DEPLOY/.fleet-cooldown-policy.canonical.sha256")"
DEPLOY_HASH="$(sha256sum "$DEPLOY/fleet-cooldown-policy.py" | cut -d' ' -f1)"
CANON_AFTER="$(tr -d ' \t\n' < "$DEPLOY/.fleet-cooldown-policy.canonical.sha256")"
if [ "$CANON_BEFORE" != "$CANON_AFTER" ]; then
    echo "FAIL: wrapper ABORT rewrote the fixture sidecar (laundering)"
    exit 1
fi
if [ "$DEPLOY_HASH" = "$CANON_AFTER" ]; then
    echo "FAIL: fixture invariant broken — corrupted file matches its sidecar"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────────
# Case 3 (fail-closed install shape): a wrapper with NO guard next to it
# (relocated copy, guard not deployed yet) must still refuse with exit 4 and
# "guard not found" on stderr — the wiring must be loud, never silent.
# ──────────────────────────────────────────────────────────────────────────
GUARDLESS="$TMP_ROOT/guardless"
mkdir -p "$GUARDLESS/scripts"
cp "$WRAPPER" "$GUARDLESS/scripts/"
OUT="$(POLICY_GUARD_SCRIPT="$DEPLOY/fleet-cooldown-policy.py" \
    bash "$GUARDLESS/scripts/verify-policy-script-deploy-hash.sh" 2>&1 1>/dev/null)"
RC=$?
echo "WRAP-NOGUARD case (wrapper without its guard): exit=$RC"
if [ "$RC" -ne 4 ]; then
    echo "FAIL: wrapper should exit 4 when its guard is missing, got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -q 'VERIFY ABORT: guard not found' <<<"$OUT"; then
    echo "FAIL: expected 'guard not found' ABORT on stderr"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

echo
echo "OK: verify-policy-script-deploy-hash.sh — WRAP-OK/WRAP-ABORT/WRAP-NOGUARD all behave as specified (SCHED-PERF-006 restart-shape integration)"
exit 0
