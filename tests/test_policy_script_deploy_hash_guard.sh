#!/usr/bin/env bash
#
# test_policy_script_deploy_hash_guard.sh — RED/GREEN proof for
# scripts/policy-script-deploy-hash-guard.sh (SCHED-PERF-006).
#
# Shape: mirrors tests/test_lint_guard.sh — a hermetic temp dir with its own
# fixture copies, no writes to the real live fleet state. The fleet's actual
# live file is read (never written) to bootstrap the fixture, and its
# sidecar hash is read (never written) to start every case from the
# canonical state.
#
# The policy script resolves its "/deployed" path from __file__, so the
# hermetic fixture IS a real deployed copy: we copy the canonical live file
# into $TMP/deploy/fleet-cooldown-policy.py and copy the sidecar alongside
# it, then point guard + policy at that deployed path with variables the
# guard supports. Mutating the fixture is the "stale variant" RED state.
#
# Requires: bash, git (repo identity only), python3, coreutils.

set -u

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GUARD="$REPO_ROOT/scripts/policy-script-deploy-hash-guard.sh"
if [ ! -f "$GUARD" ]; then
    echo "FAIL: guard not found at $GUARD" >&2
    exit 1
fi

REAL_LIVE="$HOME/.hermes/scripts/fleet-cooldown-policy.py"
REAL_SIDECAR="$HOME/.hermes/scripts/.fleet-cooldown-policy.canonical.sha256"

TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

# Fixture deploy dir: the policy script Ly-contents here behave as the live
# file; the guard is pointed at it via POLICY_GUARD_SCRIPT.
DEPLOY="$TMP_ROOT/deploy"
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

run_guard() {
    POLICY_GUARD_SCRIPT="$DEPLOY/fleet-cooldown-policy.py" \
    bash "$GUARD" 2>&1
}

# ──────────────────────────────────────────────────────────────────────
# Case 1 (GREEN): live == sidecar → guard exits 0.
# ──────────────────────────────────────────────────────────────────────
OUT="$(run_guard)"
RC=$?
echo "GREEN case (clean fixture): exit=$RC"
if [ "$RC" -ne 0 ]; then
    echo "FAIL: guard should exit 0 on a clean fixture, got $RC"
    echo "--- output ---"
    echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 2 (RED): live != sidecar → guard exits 1 with the loud marker.
# Mutates the FIXTURE (never the real live file).
# ──────────────────────────────────────────────────────────────────────
printf '\nprint("STALE_VARIANT_%s")\n' "$(date +%s)" \
    >> "$DEPLOY/fleet-cooldown-policy.py"
OUT="$(run_guard)"
RC=$?
echo "RED case (mutated fixture): exit=$RC"
if [ "$RC" -ne 1 ]; then
    echo "FAIL: guard should exit 1 on a mutated fixture, got $RC"
    echo "--- output ---"
    echo "$OUT"
    exit 1
fi
if ! grep -qF '[SCHED-PERF-006]' <<<"$OUT"; then
    echo "FAIL: expected the loud [SCHED-PERF-006] marker on stderr"
    echo "--- output ---"
    echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 3: sidecar missing on a deployed file → guard refuses (exit 4),
# no silent re-canonicalization of a possibly corrupt live.
# ──────────────────────────────────────────────────────────────────────
DEPLOY2="$TMP_ROOT/deploy_nosidecar"
mkdir -p "$DEPLOY2"
cp "$REAL_LIVE" "$DEPLOY2/fleet-cooldown-policy.py"
printf '\nprint("CORRUPT_BUT_UNSIDECARED")\n' >> "$DEPLOY2/fleet-cooldown-policy.py"
OUT="$(POLICY_GUARD_SCRIPT="$DEPLOY2/fleet-cooldown-policy.py" bash "$GUARD" 2>&1)"
RC=$?
echo "NOSIDECAR case (corrupt live, missing sidecar): exit=$RC"
if [ "$RC" -ne 4 ]; then
    echo "FAIL: guard should refuse (exit 4) on a corrupt live with no sidecar, got $RC"
    echo "--- output ---"
    echo "$OUT"
    exit 1
fi
if grep -qx "$DEPLOY2/.fleet-cooldown-policy.canonical.sha256" 2>/dev/null; then :; fi
if [ -f "$DEPLOY2/.fleet-cooldown-policy.canonical.sha256" ]; then
    echo "FAIL: guard must NOT write a sidecar for a corrupt live (laundering)"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 4: live file missing → guard exits 3.
# ──────────────────────────────────────────────────────────────────────
OUT="$(POLICY_GUARD_SCRIPT="$TMP_ROOT/deploy_missing.py" bash "$GUARD" 2>&1)"
RC=$?
echo "MISSING case (no live file): exit=$RC"
if [ "$RC" -ne 3 ]; then
    echo "FAIL: guard should exit 3 when the live file is missing, got $RC"
    echo "--- output ---"
    echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 5: python3 missing → guard exits 5 (fail-closed) instead of
# silently passing an unverifiable live.
# ──────────────────────────────────────────────────────────────────────
# Build a minimal PATH that has bash + the coreutils the guard needs
# (dirname, tr) but NO python3 — an empty PATH kills the `bash` invocation
# itself (exit 127) rather than the guard's own check.
NOPYPATH="$TMP_ROOT/nopy_path"
mkdir -p "$NOPYPATH"
for tool in bash dirname tr; do
    ln -s "$(command -v "$tool")" "$NOPYPATH/$tool"
done
OUT="$(POLICY_GUARD_SCRIPT="$DEPLOY/fleet-cooldown-policy.py" \
       PATH="$NOPYPATH" bash "$GUARD" 2>&1)"
RC=$?
echo "NOPYPATH case (python3 unavailable): exit=$RC"
if [ "$RC" -ne 5 ]; then
    echo "FAIL: guard should exit 5 when python3 is unavailable, got $RC"
    echo "--- output ---"
    echo "$OUT"
    exit 1
fi

echo
echo "OK: policy-script-deploy-hash-guard.sh — GREEN/RED/NOSIDECAR/MISSING/NOPYPATH all behave as specified (SCHED-PERF-006)"
exit 0
