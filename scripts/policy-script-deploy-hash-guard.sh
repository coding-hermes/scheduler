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
#   0  verified clean — deploy hash matches the canonical sidecar AND the
#      script's own --verify reported no drift
#   1  deploy-hash MISMATCH — live diverged from the sidecar; consumer
#      deploy/restart would CLOBBER the operator's live fix. LOUD
#      [SCHED-PERF-006] line on stderr.
#   2  DRIFT — the script's own --verify failed while the deploy hash is
#      CLEAN: pin drift (MISMATCH lines, one per drifted lane) and/or
#      lane-file parity drift (SCHED-GAP-16951). One drift verdict on
#      stderr with the drifted lane names as evidence. A consumer walking
#      past drift is how the clobber happens — a restart re-pins from
#      fleet.toml, so drifting pins get enforced by the very next deploy.
#      The pins tripwire still owns the FIX; this guard owns refusing to
#      bless a deploy while drift stands.
#   3  live file missing
#   4  sidecar missing while a live file exists (bootstrap refusal: the
#      policy script's own --verify would silently WRITE a sidecar from a
#      possibly-corrupt live, laundering the corruption; an operator who
#      just deployed intentionally runs --update-canonical to clear this)
#   5  python3 missing, live unreadable, --verify crashed, or --verify
#      exited non-zero with NO recognizable drift verdict
#      (fail-closed — an unverifiable live is not a clean live)
#
# ONE VERDICT LAW (SCHED-GAP-16951)
# ---------------------------------
# The guard prints exactly one drift verdict, always consistent with its
# exit code: 0 means no drift, 2 means drift, and "0 mismatches" and
# "drift detected" never co-occur on this surface. The inner --verify's
# own summary line is NEVER forwarded — it miscounts parity drift as pin
# mismatches (live 2026-11-08: "PARITY MISMATCH: db lanes=846
# file blocks=599" next to "0 pin mismatch(es) — drift detected", rc=0),
# which made this instrument report three contradictory verdicts per run
# and taught every commit-hook reader to ignore it.
#
# RED/GREEN proof: tests/test_policy_script_deploy_hash_guard.sh

set -u

LIVE="${POLICY_GUARD_SCRIPT:-$HOME/.hermes/scripts/fleet-cooldown-policy.py}"
SIDECAR="$(dirname "$LIVE")/.fleet-cooldown-policy.canonical.sha256"

loud() {
    echo "[SCHED-PERF-006] policy script deploy hash MISMATCH: $1" >&2
}

# SCHED-GAP-16951: a drift verdict is NOT a hash mismatch — it gets its own
# line shape so a clean hash never reads as "MISMATCH" and vice versa.
verdict() {
    echo "[SCHED-PERF-006] $1" >&2
}

# SCHED-GAP-16951 ONE VERDICT LAW, part 2: the inner --verify's own
# "N pin mismatch(es) — drift detected" summary line is NEVER forwarded on
# ANY path (hash-mismatch, verifier contradiction, or fail-closed), because
# that line is itself a second, differently-derived verdict and it
# miscounts parity drift as pin mismatches. Everything else the inner
# verify printed is forwarded verbatim so no evidence is lost.
emit_verify_out() {
    printf '%s\n' "$VERIFY_OUT" \
        | grep -vE '^[[:space:]]*[0-9]+ pin mismatch\(es\)[[:space:]]*([-—:]+[[:space:]]*)?drift detected[[:space:]]*$' \
        || true
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
        emit_verify_out >&2
    fi
    exit 1
fi

# Hashes agree. If the script's own --verify still failed, classify the
# failure from its output (ONE VERDICT LAW, SCHED-GAP-16951): every
# non-clean outcome exits non-zero and the verdict line always matches
# the exit code. Drift evidence lines are forwarded verbatim; the inner
# "N pin mismatch(es) — drift detected" summary is NOT (it miscounts
# parity drift as pin mismatches, and two verdicts must never co-occur).
if [ "$VERIFY_RC" -ne 0 ]; then
    if echo "$VERIFY_OUT" | grep -q 'DEPLOY HASH MISMATCH'; then
        # The script disagrees with our direct check (its sidecar view differs).
        # A verifier contradiction is itself fail-closed territory.
        loud "the policy script's own --verify reports a hash mismatch that the sidecar next to the deployed file does not"
        emit_verify_out >&2
        exit 1
    fi
    DRIFT_EVIDENCE="$(echo "$VERIFY_OUT" | grep -E '^(MISMATCH |PARITY MISMATCH:)' || true)"
    if [ -n "$DRIFT_EVIDENCE" ]; then
        verdict "DRIFT detected by the policy script's own --verify (deploy hash clean) — refusing:"
        echo "$DRIFT_EVIDENCE" >&2
        if echo "$DRIFT_EVIDENCE" | grep -q '^PARITY MISMATCH:'; then
            echo "  units: db lanes = live /api/v1/projects rows (paginated); file blocks = [[projects]] blocks in ~/.hermes/fleet.toml (SCHED-GAP-1662)" >&2
        fi
        exit 2
    fi
    # rc != 0 with no recognizable drift marker: the verify run itself
    # broke (traceback, crash, unknown output). An unverifiable live is
    # not a clean live — fail closed instead of blessing it with rc=0.
    verdict "--verify exited $VERIFY_RC with no drift verdict and no hash mismatch — unverifiable, refusing (fail-closed):"
    emit_verify_out >&2
    exit 5
fi

echo "[SCHED-PERF-006] policy script deploy hash OK: $LIVE_HASH matches canonical — pins+parity clean, no drift"
exit 0
