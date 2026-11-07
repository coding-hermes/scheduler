#!/usr/bin/env bash
# QA-CHS-187 error-path chaos probe: prove the real cmd/schedulerd binary
# fails LOUDLY on a bad --config instead of booting silently or dying with
# a bare stack that names nothing.
#
# ch:trace row=QA-CHS-187 spec=none:qa-row test=.qa/chaos-probes/errorpath.sh::manual-red-green doc=none:scope prompt=QA-CHS-187 evidence=none:worker-local witness=none:foreman-run verdict=none:pending-foreman commit=none:worker-sha memory=none:foreman-closeout
#
# Contract (board row QA-CHS-187):
#   - exercises the real cmd/schedulerd binary only (never backfill-commit-signals
#     or any other batch CLI);
#   - runs in a throwaway HOME with temp config/DB/log paths — no live daemon
#     restart, no live scheduler DB access (bad configs die at foreman-routing
#     resolution in cmd/schedulerd/main.go, before database.InitDB);
#   - covers the ABSENT-config case (--config names a missing file) and the
#     INVALID-config case (--config names an unparsable TOML file);
#   - the probe exits 0 only when BOTH cases make schedulerd exit non-zero
#     with a diagnostic that names the exact config path (internal/config
#     loader.go prefixes failures with "stat config <path>" / "decode config <path>");
#   - the probe exits non-zero when a case exits 0 cleanly with no diagnostic
#     (silent boot) or when the output never names the config path (bare stack).
# The exact binary and config path used are printed for every case.
#
# Usage: errorpath.sh [path-to-schedulerd]
#   Without $1 the probe uses <repo-root>/bin/schedulerd and refuses to run
#   if it is missing (build it with: go build -o bin/schedulerd ./cmd/schedulerd/).
set -u

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BIN="${1:-$REPO_ROOT/bin/schedulerd}"

if [ ! -x "$BIN" ]; then
    echo "PROBE-ERROR: binary not executable: $BIN" >&2
    echo "PROBE-ERROR: pass a schedulerd path as \$1, or build one: go build -o bin/schedulerd ./cmd/schedulerd/" >&2
    exit 2
fi

PROBE_TMP="$(mktemp -d)"
trap 'rm -rf "$PROBE_TMP"' EXIT
# Throwaway HOME: every HOME-derived default (--hooks-file, --foreman-home,
# --session-db, the derived log path) resolves inside the probe sandbox.
export HOME="$PROBE_TMP/home"
mkdir -p "$HOME"

# Scrub the clock env: an ambient SCHEDULER_TIME_MODE=sim must not change the
# boot failure path this probe measures (sim without --simulate is a different
# FATAL that does not name the config).
unset SCHEDULER_TIME_MODE SCHEDULER_TIME_SCALE SCHEDULER_TIME_START SCHEDULER_TIME_AUTOADVANCE

ABSENT_CFG="$PROBE_TMP/absent/fleet.toml"    # deliberately never created
INVALID_CFG="$PROBE_TMP/invalid/fleet.toml"
DB_PATH="$PROBE_TMP/scheduler.db"            # temp DB only; never the live scheduler DB
LOG_PATH="$PROBE_TMP/daemon.log"
mkdir -p "$(dirname "$INVALID_CFG")"
# Invalid TOML: an unclosed table header is a guaranteed decode error.
echo '[scheduler' > "$INVALID_CFG"

echo "PROBE: binary=$BIN"
echo "PROBE: absent-config path=$ABSENT_CFG"
echo "PROBE: invalid-config path=$INVALID_CFG"

FAILURES=0

# expect_loud_failure <label> <config-path>
# Green iff schedulerd exits non-zero (not via probe timeout) AND its output
# names the exact config path. Exit 0 with no diagnostic, a probe timeout
# (daemon booted and hung), or output that never names the config are red.
expect_loud_failure() {
    label="$1"
    cfg="$2"
    echo "PROBE: case=$label running schedulerd..."
    out="$(timeout 30 "$BIN" \
        --config "$cfg" \
        --db "$DB_PATH" \
        --log-file "$LOG_PATH" \
        --listen 127.0.0.1:0 \
        --gateway-url "" \
        --duckbrain-url http://127.0.0.1:9 2>&1)"
    rc=$?
    echo "$out" | sed 's/^/  daemon: /'
    if [ "$rc" -eq 124 ]; then
        echo "PROBE: FAIL case=$label — daemon did not exit within 30s (timeout: it booted instead of failing loudly)"
        FAILURES=$((FAILURES + 1))
        return
    fi
    if [ "$rc" -eq 0 ]; then
        echo "PROBE: FAIL case=$label — daemon exited 0 cleanly with no boot diagnostic"
        FAILURES=$((FAILURES + 1))
        return
    fi
    case "$out" in
        *"$cfg"*) ;;
        *)
            echo "PROBE: FAIL case=$label — exit=$rc but no diagnostic names the config path (bare stack or silent stderr)"
            FAILURES=$((FAILURES + 1))
            return
            ;;
    esac
    echo "PROBE: PASS case=$label — exit=$rc with a diagnostic naming the config path"
}

expect_loud_failure absent-config "$ABSENT_CFG"
expect_loud_failure invalid-config "$INVALID_CFG"

if [ "$FAILURES" -gt 0 ]; then
    echo "PROBE: RESULT=FAIL failures=$FAILURES binary=$BIN"
    exit 1
fi
echo "PROBE: RESULT=PASS — both error paths fail loudly and name the config binary=$BIN"
exit 0
