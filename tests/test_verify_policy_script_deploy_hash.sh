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
#   clean fixture    -> exit 0, "deploy hash OK" + "no drift" text, abort
#                       markers OFF stdout
#   seeded pin drift -> exit 4 (documented ABORT), "VERIFY ABORT" AND the
#                       guard's drift verdict naming the drifted lane on
#                       STDERR, not stdout (SCHED-GAP-16951 ONE VERDICT LAW)
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
# SCHED-GAP-16951: the clean and drift arms use a STUB policy script whose
# --verify emits the documented output shapes — the live fleet's pin state
# cannot be seeded hermetically, and the old real-file OK case asserted
# live-fleet cleanliness (it passed while the wrapper blessed real drift
# with rc=0). The real file keeps ONE case: hash-corruption ABORT, which
# fires on the direct hash check before any verify classification and is
# drift-state independent.
#
# Note on the exit contract: this test itself exits 0 when ALL wrapper
# branches (OK and both ABORTs) behave as documented — an ABORT inside a
# fixture is the expected outcome, not a failure of the test.
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

# ── SCHED-GAP-16951 stub fixture (same shape as the guard test) ──────────
DEPLOY_STUB="$TMP_ROOT/deploy_stub"
mkdir -p "$DEPLOY_STUB"
write_stub() {
    cat > "$DEPLOY_STUB/fleet-cooldown-policy.py" <<'PY'
import os, sys
mode = os.environ.get('FALSIFIER_MODE', 'clean')
if mode == 'clean':
    print('DEPLOY HASH: stub-hash matches canonical')
    print('PARITY OK: db lanes=2 file blocks=2')
    print('OK: 2 canonical pin(s) agree across fleet.toml and the live DB; no tasks-admission lane armed')
    sys.exit(0)
if mode == 'pin_drift':
    print('DEPLOY HASH: stub-hash matches canonical')
    print('MISMATCH seeded-lane cooldown_s: db=7200 toml=21600 (canonical pin=21600)')
    print('PARITY OK: db lanes=2 file blocks=2')
    print('')
    print('1 pin mismatch(es) - drift detected')
    sys.exit(1)
sys.exit(9)
PY
    sha256sum "$DEPLOY_STUB/fleet-cooldown-policy.py" | cut -d' ' -f1 \
        > "$DEPLOY_STUB/.fleet-cooldown-policy.canonical.sha256"
}

run_stub_wrapper() {
    FALSIFIER_MODE="$1" \
    POLICY_GUARD_SCRIPT="$DEPLOY_STUB/fleet-cooldown-policy.py" \
        bash "$WRAPPER" >"$STDOUT" 2>"$STDERR"
}

# ──────────────────────────────────────────────────────────────────────────
# Case 1 (OK, stub): verify clean → wrapper exits 0, hash-OK + "no drift"
# text on stdout, no ABORT markers anywhere.
# ──────────────────────────────────────────────────────────────────────────
write_stub
run_stub_wrapper clean
RC=$?
echo "WRAP-OK case (stub, verify clean): exit=$RC"
if [ "$RC" -ne 0 ]; then
    echo "FAIL: wrapper should exit 0 on a clean verify, got $RC"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if ! grep -q 'deploy hash OK' "$STDOUT"; then
    echo "FAIL: expected 'deploy hash OK' on the wrapper's stdout"
    echo "--- stdout ---"; cat "$STDOUT"
    exit 1
fi
if ! grep -q 'no drift' "$STDOUT"; then
    echo "FAIL: rc=0 must carry the 'no drift' verdict on stdout (ONE VERDICT LAW)"
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
# Case 2 (DRIFT ABORT, stub): seeded pin drift → wrapper exits 4 with the
# "VERIFY ABORT" marker AND the guard's drift verdict naming the drifted
# lane on STDERR — NOT stdout. Sidecar untouched (no laundering).
# ──────────────────────────────────────────────────────────────────────────
run_stub_wrapper pin_drift
RC=$?
echo "WRAP-DRIFT case (stub, seeded pin drift): exit=$RC"
if [ "$RC" -eq 0 ]; then
    echo "FAIL: wrapper must NOT exit 0 on seeded drift (0 = no drift — the old lie)"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if [ "$RC" -ne 4 ]; then
    echo "FAIL: wrapper should exit 4 (documented ABORT) on seeded drift, got $RC"
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
    echo "FAIL: expected the guard's [SCHED-PERF-006] drift verdict on STDERR"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if ! grep -q 'seeded-lane' "$STDERR"; then
    echo "FAIL: the drift verdict must name the drifted lane (seeded-lane)"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if grep -q 'drift detected' "$STDOUT" "$STDERR"; then
    echo "FAIL: the inner summary 'N pin mismatch(es) — drift detected' must not surface"
    echo "--- stdout ---"; cat "$STDOUT"
    echo "--- stderr ---"; cat "$STDERR"
    exit 1
fi
if grep -q 'VERIFY ABORT' "$STDOUT"; then
    echo "FAIL: 'VERIFY ABORT' leaked onto stdout (verify block cannot stream-split)"
    echo "--- stdout ---"; cat "$STDOUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────────
# Case 3 (ABORT, real file): corrupted fixture → wrapper exits 4, with the
# "VERIFY ABORT" marker AND the guard's "[SCHED-PERF-006]" marker on
# STDERR. Mutates the FIXTURE only; the real live file is never touched.
# Fires on the direct hash check (before any verify classification), so it
# is drift-state independent.
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
# Case 4 (fail-closed install shape): a wrapper with NO guard next to it
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
echo "OK: verify-policy-script-deploy-hash.sh — WRAP-OK/WRAP-DRIFT/WRAP-ABORT/WRAP-NOGUARD all behave as specified (SCHED-PERF-006 restart-shape integration + SCHED-GAP-16951 ONE VERDICT LAW)"
exit 0
