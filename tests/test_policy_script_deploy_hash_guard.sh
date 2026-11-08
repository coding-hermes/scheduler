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
# SCHED-GAP-16951 (ONE VERDICT LAW): the drift arms are driven by a STUB
# policy script whose --verify output reproduces the documented shapes
# (clean / pin drift / parity drift / crash). The real fleet's live pin
# state cannot be seeded hermetically (the live --verify reads the live
# scheduler API + fleet.toml), so the deterministic clean-vs-drift arms
# use the stub; the REAL file gets one consistency case: its exit code
# must always match its verdict text (0 <=> "no drift", 2 <=> "DRIFT
# detected" + drift evidence), whatever the live fleet state is. Each
# stub arm is a positive control: the fixture sidecar is pinned to the
# stub's real hash so only the --verify verdict can move the outcome, and
# the seeded-drift arms assert the guard's rc AND its output invariants
# (one verdict line, no forwarded inner summary, no co-occurring
# "0 pin mismatch(es)" + "drift detected").
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

# ── SCHED-GAP-16951 stub fixture ─────────────────────────────────────────
# A deployed-copy-shaped policy script whose --verify emits the documented
# output shape for its FALSIFIER_MODE. The sidecar next to it is pinned to
# the stub's real sha so the guard's direct hash check always passes and
# ONLY the verify classification can move the outcome (positive control).
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
if mode == 'parity_drift':
    print('DEPLOY HASH: stub-hash matches canonical')
    print('PARITY MISMATCH: db lanes=2 file blocks=1')
    print('')
    print('0 pin mismatch(es) - drift detected')
    sys.exit(1)
if mode == 'crash':
    raise RuntimeError('stub verify crash')
sys.exit(9)
PY
    sha256sum "$DEPLOY_STUB/fleet-cooldown-policy.py" | cut -d' ' -f1 \
        > "$DEPLOY_STUB/.fleet-cooldown-policy.canonical.sha256"
}

run_stub_guard() {
    FALSIFIER_MODE="$1" \
    POLICY_GUARD_SCRIPT="$DEPLOY_STUB/fleet-cooldown-policy.py" \
    bash "$GUARD" 2>&1
}

# ──────────────────────────────────────────────────────────────────────
# Case 1 (GREEN, stub): verify clean → guard exits 0 and says "no drift".
# The exit code and the verdict text must agree (ONE VERDICT LAW).
# ──────────────────────────────────────────────────────────────────────
write_stub
OUT="$(run_stub_guard clean)"
RC=$?
echo "GREEN case (stub, verify clean): exit=$RC"
if [ "$RC" -ne 0 ]; then
    echo "FAIL: guard should exit 0 on a clean verify, got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -q 'no drift' <<<"$OUT"; then
    echo "FAIL: clean run must print 'no drift' (verdict must match rc=0)"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if grep -q 'drift detected' <<<"$OUT"; then
    echo "FAIL: 'drift detected' must never co-occur with rc=0"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 2 (RED, stub): seeded PIN drift (one lane's cooldown changed on one
# side only) → guard exits 2, names the drifted lane, forwards the
# MISMATCH evidence, and does NOT forward the inner summary or the old
# contradictory banner. Exactly ONE verdict line.
# ──────────────────────────────────────────────────────────────────────
OUT="$(run_stub_guard pin_drift)"
RC=$?
echo "PIN-DRIFT case (stub, seeded cooldown drift on seeded-lane): exit=$RC"
if [ "$RC" -ne 2 ]; then
    echo "FAIL: guard should exit 2 on seeded pin drift (0 = no drift, non-zero = drift), got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -q 'DRIFT detected' <<<"$OUT"; then
    echo "FAIL: seeded drift must produce the DRIFT detected verdict"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -q '^MISMATCH seeded-lane cooldown_s' <<<"$OUT"; then
    echo "FAIL: drift verdict must name the drifted lane (seeded-lane) as evidence"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if grep -q -- '--verify exited 1 on PIN drift' <<<"$OUT"; then
    echo "FAIL: the old contradictory banner must be gone"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if grep -q 'drift detected' <<<"$OUT"; then
    echo "FAIL: the inner summary 'N pin mismatch(es) — drift detected' must not be forwarded"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if grep -q 'PARITY OK' <<<"$OUT"; then
    echo "FAIL: exactly one verdict — a pin-drift verdict must not carry the parity line"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
N_VERDICTS="$(grep -c '^\[SCHED-PERF-006\]' <<<"$OUT")"
if [ "$N_VERDICTS" -ne 1 ]; then
    echo "FAIL: exactly one [SCHED-PERF-006] verdict line expected, got $N_VERDICTS"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 3 (RED, stub): seeded PARITY drift → guard exits 2, forwards the
# PARITY MISMATCH line with its units note, and the inner "0 pin
# mismatch(es) — drift detected" contradiction is never forwarded.
# ──────────────────────────────────────────────────────────────────────
OUT="$(run_stub_guard parity_drift)"
RC=$?
echo "PARITY-DRIFT case (stub, seeded block-count drift): exit=$RC"
if [ "$RC" -ne 2 ]; then
    echo "FAIL: guard should exit 2 on seeded parity drift, got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -q '^PARITY MISMATCH: db lanes=2 file blocks=1' <<<"$OUT"; then
    echo "FAIL: parity drift evidence line missing"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -q 'units: db lanes' <<<"$OUT"; then
    echo "FAIL: a parity verdict must name the units of both counts"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if grep -q '0 pin mismatch' <<<"$OUT"; then
    echo "FAIL: '0 pin mismatch(es)' must never be forwarded next to a drift verdict"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if grep -q 'drift detected' <<<"$OUT"; then
    echo "FAIL: the inner summary line must not be forwarded"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 4 (fail-closed, stub): --verify crashed with no recognizable
# verdict → guard exits 5. rc=0 on an unverifiable live was the second
# lie this guard told (SCHED-GAP-16951).
# ──────────────────────────────────────────────────────────────────────
OUT="$(run_stub_guard crash)"
RC=$?
echo "CRASH case (stub, verify crashes): exit=$RC"
if [ "$RC" -ne 5 ]; then
    echo "FAIL: guard should exit 5 (fail-closed) when --verify crashes without a verdict, got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 5 (REAL FILE, consistency): the real deployed policy script, real
# live fleet state. The guard's rc must agree with its verdict text,
# whichever way the live fleet points:
#   rc=0 → "no drift" present, "drift detected" absent
#   rc=2 → "DRIFT detected" present with MISMATCH/PARITY evidence lines
# (The old flat assertion rc==0 asserted live-fleet cleanliness, which is
# live state, not fixture state — it passed while the guard exited 0 on
# real drift. SCHED-GAP-16951.)
# ──────────────────────────────────────────────────────────────────────
OUT="$(run_guard)"
RC=$?
echo "REAL-FILE consistency case (live fleet state): exit=$RC"
if [ "$RC" -eq 0 ]; then
    if ! grep -q 'no drift' <<<"$OUT"; then
        echo "FAIL: rc=0 must say 'no drift'"
        echo "--- output ---"; echo "$OUT"
        exit 1
    fi
    if grep -q 'drift detected' <<<"$OUT"; then
        echo "FAIL: rc=0 with a 'drift detected' line — verdict contradicts exit code"
        echo "--- output ---"; echo "$OUT"
        exit 1
    fi
elif [ "$RC" -eq 2 ]; then
    if ! grep -q 'DRIFT detected' <<<"$OUT"; then
        echo "FAIL: rc=2 must carry the DRIFT detected verdict"
        echo "--- output ---"; echo "$OUT"
        exit 1
    fi
    if ! grep -qE '^(MISMATCH |PARITY MISMATCH:)' <<<"$OUT"; then
        echo "FAIL: rc=2 must carry drift evidence lines (drifted lane names / parity counts)"
        echo "--- output ---"; echo "$OUT"
        exit 1
    fi
    if grep -q '0 pin mismatch' <<<"$OUT"; then
        echo "FAIL: rc=2 with '0 pin mismatch(es)' — the contradiction is back"
        echo "--- output ---"; echo "$OUT"
        exit 1
    fi
else
    echo "FAIL: real-file run must exit 0 (clean) or 2 (drift), got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 6 (RED, real file): live != sidecar → guard exits 1 with the loud
# marker. Mutates the FIXTURE (never the real live file). Fires on the
# direct hash check, before any verify classification — drift-state
# independent.
# ──────────────────────────────────────────────────────────────────────
printf '\nprint("STALE_VARIANT_%s")\n' "$(date +%s)" \
    >> "$DEPLOY/fleet-cooldown-policy.py"
OUT="$(run_guard)"
RC=$?
echo "HASH-RED case (mutated fixture): exit=$RC"
if [ "$RC" -ne 1 ]; then
    echo "FAIL: guard should exit 1 on a mutated fixture, got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if ! grep -qF '[SCHED-PERF-006]' <<<"$OUT"; then
    echo "FAIL: expected the loud [SCHED-PERF-006] marker on stderr"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 7: sidecar missing on a deployed file → guard refuses (exit 4),
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
    echo "--- output ---"; echo "$OUT"
    exit 1
fi
if [ -f "$DEPLOY2/.fleet-cooldown-policy.canonical.sha256" ]; then
    echo "FAIL: guard must NOT write a sidecar for a corrupt live (laundering)"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 8: live file missing → guard exits 3.
# ──────────────────────────────────────────────────────────────────────
OUT="$(POLICY_GUARD_SCRIPT="$TMP_ROOT/deploy_missing.py" bash "$GUARD" 2>&1)"
RC=$?
echo "MISSING case (no live file): exit=$RC"
if [ "$RC" -ne 3 ]; then
    echo "FAIL: guard should exit 3 when the live file is missing, got $RC"
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# Case 9: python3 missing → guard exits 5 (fail-closed) instead of
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
    echo "--- output ---"; echo "$OUT"
    exit 1
fi

echo
echo "OK: policy-script-deploy-hash-guard.sh — GREEN/PIN-DRIFT/PARITY-DRIFT/CRASH/REAL-FILE/HASH-RED/NOSIDECAR/MISSING/NOPYPATH all behave as specified (SCHED-PERF-006 + SCHED-GAP-16951 ONE VERDICT LAW)"
exit 0
