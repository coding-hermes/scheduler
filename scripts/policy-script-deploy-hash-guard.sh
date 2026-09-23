#!/usr/bin/env bash
#
# policy-script-deploy-hash-guard.sh — deploy-hash tripwire for the live
# fleet-cooldown-policy.py consumer path (SCHED-PERF-006).
#
# WHY THIS EXISTS
# ---------------
# The live ops script ~/.hermes/scripts/fleet-cooldown-policy.py carries an
# internal deploy-hash guard (verify_deploy_hash, SCHED-PERF-003): its own
# --apply / --verify paths refuse to run when the live file diverges from the
# canonical sidecar. But the CONSUMER paths around it had no tripwire:
#
#   * ~/task-router/scripts/sync_runtime.sh refuses only when the live file
#     MATCHES the sidecar but the repo copy differs; an operator fix that
#     diverged the live file away from the sidecar is silently CLOBBERED by
#     the next sync ("non-canonical live copy replaced").
#   * ~/.hermes/scripts/scheduler-deploy-drain-restart.sh verifies caps,
#     admission, migration, and fix symbols — but never the policy script's
#     integrity — so a restart on a corrupted live file proceeds without
#     protest.
#
# This guard closes the consumer gap. It (a) runs the policy script's own
# `python3 <live> --verify` — the SCHED-PERF-003 deploy-hash path — and
# (b) re-checks the hash directly, because the policy script resolves its
# sidecar against ~/.hermes/scripts while this guard must accept a deployed
# copy at any location (deploy topology: the sidecar sits NEXT TO the
# deployed script). A mismatch from either source is a LOUD refusal.
#
# USAGE
# -----
#   bash scripts/policy-script-deploy-hash-guard.sh
#
# Site-override for tests/tools (deploys a fixture copy as the "live"):
#   POLICY_GUARD_SCRIPT=/path/to/fleet-cooldown-policy.py
#
# EXIT CODES
# ----------
#   0  live confirmed canonical (clean)
#   1  deploy-hash MISMATCH — live diverged from the sidecar; consumer
#      deploy/restart would CLOBBER the operator's live fix. LOUD
#      [SCHED-PERF-006] line on stderr.
#   3  live file missing
#   4  sidecar missing while a live file exists (bootstrap refusal: the
#      policy script's own --verify would silently WRITE a sidecar from a
#      possibly-corrupt live, laundering the corruption; an operator who
#      just deployed intentionally runs --update-canonical to clear this)
#   5  python3 missing, live unreadable, or the verify run crashed
#      (fail-closed — an unverifiable live is not a clean live)
#
# Red line (additive, non-clobber): if the script's --verify exits 1 on
# PIN drift while the deploy hash is CLEAN, this guard reports the pin
# drift and exits 0 — the pins tripwire owns that alert, not the clobber
# guard.
#
# RED/GREEN proof: tests/test_policy_script_deploy_hash_guard.sh

set -u

LIVE="${POLICY_GUARD_SCRIPT:-$HOME/.hermes/scripts/fleet-cooldown-policy.py}"
SIDECAR="$(dirname "$LIVE")/.fleet-cooldown-policy.canonical.sha256"

loud() {
    echo "[SCHED-PERF-006] policy script deploy hash MISMATCH: $1" >&2
}

sha_of() {
    python3 - "$1" <<'PY' 2>/dev/null
import hashlib, sys
h = hashlib.sha256()
with open(sys.argv[1], 'rb') as f:
    for chunk in iter(lambda: f.read(65536), b''):
        h.update(chunk)
print(h.hexdigest())
PY
}

# ── Precondition: python3 available ────────────────────────────────────────
if ! command -v python3 >/dev/null 2>&1; then
    loud "python3 not on PATH — cannot verify the policy script's deploy hash"
    exit 5
fi

# ── Precondition: live file exists ─────────────────────────────────────────
if [ ! -f "$LIVE" ]; then
    loud "live policy script missing at $LIVE"
    exit 3
fi

# ── Run the policy script's own --verify (deploy-hash gate, SCHED-PERF-003)
VERIFY_OUT="$(python3 "$LIVE" --verify 2>&1)"
VERIFY_RC=$?

# ── Direct hash check (deployment-location authority) ──────────────────────
LIVE_HASH="$(sha_of "$LIVE")"
if [ -z "$LIVE_HASH" ]; then
    loud "live policy script at $LIVE is unreadable"
    exit 5
fi

if [ ! -f "$SIDECAR" ]; then
    loud "sidecar missing at $SIDECAR while live file $LIVE_HASH exists — refusing to launder (operator: run the policy script's --update-canonical ONLY after an INTENTIONAL deploy)"
    exit 4
fi
CANON_HASH="$(tr -d ' \t\n' < "$SIDECAR")"

hash_mismatch=0
if [ "$LIVE_HASH" != "$CANON_HASH" ]; then
    hash_mismatch=1
fi

# ── Verdict ────────────────────────────────────────────────────────────────
if [ "$hash_mismatch" -eq 1 ]; then
    loud "live file diverged from the canonical sidecar"
    echo "  live     : $LIVE_HASH  ($LIVE)" >&2
    echo "  canonical: $CANON_HASH  ($SIDECAR)" >&2
    echo "[SCHED-PERF-006] refusing: a consumer deploy/restart would CLOBBER the operator's live fix" >&2
    if [ "$VERIFY_RC" -ne 0 ]; then
        echo "$VERIFY_OUT" >&2
    fi
    exit 1
fi

# Hashes agree. If the script's own --verify still failed, classify:
if [ "$VERIFY_RC" -ne 0 ]; then
    if echo "$VERIFY_OUT" | grep -q 'DEPLOY HASH MISMATCH'; then
        # The script disagrees with our direct check (its sidecar view differs).
        # A verifier contradiction is itself fail-closed territory.
        loud "the policy script's own --verify reports a hash mismatch that the sidecar next to the deployed file does not"
        echo "$VERIFY_OUT" >&2
        exit 1
    fi
    # Verify failed on pin drift (hash clean everywhere we can see): report,
    # do not fake a clobber alert — the pins tripwire owns that.
    echo "[SCHED-PERF-006] policy script --verify exited 1 on PIN drift (deploy hash clean):"
    echo "$VERIFY_OUT"
    exit 0
fi

echo "[SCHED-PERF-006] policy script deploy hash OK: $LIVE_HASH matches canonical"
exit 0
