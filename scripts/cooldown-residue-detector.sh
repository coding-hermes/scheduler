#!/usr/bin/env bash
# cooldown-residue-detector.sh — the SCHEDULED, READ-ONLY wake-residue
# detector (SCHED-GAP-1670). Deployed to ~/.hermes/scripts/ and driven by
# deploy/cooldown-residue.timer (daily, systemd --user).
#
# WHY THIS EXISTS
# ---------------
# A lane whose LIVE cooldown sits below its own operator pin runs hotter than
# the cadence recorded for it — the fleet's policy script calls this class
# "wake residue, NOT intent". The correction tool
# (~/.hermes/scripts/fleet-cooldown-policy.py) can revert it, but its useful
# path MUTATES fleet state, so it must stay an operator action and must never
# be scheduled. Nothing else watched the class, so it accumulated silently: the
# 2026-09-28 measurement found eleven enabled lanes below their own pin, the
# worst four (python-audit-*) burning ~25 zero-commit ticks each per day.
#
# This wrapper is the scheduled piece. It builds (once, cached) and runs
# `cmd/cooldown-residue`, which reads the scheduler DB mode=ro plus the
# fleet.toml seed and reports. Nothing here writes fleet state: no PUT, no
# --apply, no migration, no DB write. The only file it creates is the cached
# binary under ${HOME}/.hermes/bin/.
#
# USAGE / SCHEDULE
#   systemctl --user enable --now cooldown-residue.timer   # daily
#   systemctl --user list-timers cooldown-residue.timer
#   journalctl --user -u cooldown-residue.service -n 50    # last report
#   bash ~/.hermes/scripts/cooldown-residue-detector.sh    # run by hand
#
# EXIT CODES (the detector's own, passed through; 3 is wrapper-level)
#   0  clean — no enabled lane sits below its own pin
#   1  residue found (ALERT lines on stdout; the mutating remedy is the
#      operator's fleet-cooldown-policy.py --apply run)
#   2  the detector could not read state (missing/unreadable DB) — never
#      reported as clean, so a wrong --db cannot look healthy
#   3  the wrapper could not build or locate the detector (broken checkout,
#      no Go toolchain) — an environment fault, distinct from both above
#
# ENV OVERRIDES (every path is overridable so the script is testable without
# touching live infra):
#   COOLDOWN_RESIDUE_DB          default ${HOME}/.hermes/coding-hermes/scheduler.db
#   COOLDOWN_RESIDUE_FLEET_TOML  default ${HOME}/.hermes/fleet.toml
#   COOLDOWN_RESIDUE_BIN         default ${HOME}/.hermes/bin/cooldown-residue
#   COOLDOWN_RESIDUE_QUIET       "1" suppresses the clean report (default: print it)
#   SCHEDULER_REPO               repo checkout to build from (default: probe)
#   GO_BIN                       go binary (default: first `go` on PATH)

set -u

DB="${COOLDOWN_RESIDUE_DB:-$HOME/.hermes/coding-hermes/scheduler.db}"
FLEET_TOML="${COOLDOWN_RESIDUE_FLEET_TOML:-$HOME/.hermes/fleet.toml}"
BIN="${COOLDOWN_RESIDUE_BIN:-$HOME/.hermes/bin/cooldown-residue}"
QUIET="${COOLDOWN_RESIDUE_QUIET:-0}"

log() { printf '%s cooldown-residue-detector: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

# resolve_repo prints the checkout holding cmd/cooldown-residue, or nothing.
# $SCHEDULER_REPO wins; otherwise the two conventional checkout roots are
# probed, so the same script works on a box that cloned flat or nested.
resolve_repo() {
    if [ -n "${SCHEDULER_REPO:-}" ]; then
        printf '%s\n' "$SCHEDULER_REPO"
        return 0
    fi
    for candidate in "${HOME}/coding-hermes-scheduler/coding-herms-scheduler" "${HOME}/coding-hermes-scheduler"; do
        if [ -d "${candidate}/cmd/cooldown-residue" ]; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    return 0
}

fail_env() {
    log "ERROR: $*"
    exit 3
}

REPO="$(resolve_repo)"

# Build only when needed: no binary yet, or a source file newer than it. The
# staleness probe covers the packages the command depends on.
need_build=0
if [ ! -x "$BIN" ]; then
    need_build=1
elif [ -n "$REPO" ] && [ -d "$REPO/cmd/cooldown-residue" ]; then
    if [ -n "$(find "$REPO/cmd/cooldown-residue" "$REPO/internal" -name '*.go' -newer "$BIN" -print -quit 2>/dev/null)" ]; then
        need_build=1
    fi
fi

if [ "$need_build" -eq 1 ]; then
    [ -n "$REPO" ] || fail_env "no checkout found (set SCHEDULER_REPO): cannot build ${BIN}"
    [ -d "$REPO/cmd/cooldown-residue" ] || fail_env "checkout ${REPO} has no cmd/cooldown-residue (stale clone?)"
    if [ -n "${GO_BIN:-}" ]; then
        go_cmd="$GO_BIN"
    else
        go_cmd="$(command -v go || true)"
    fi
    [ -n "$go_cmd" ] || fail_env "no go toolchain on PATH (set GO_BIN) and ${BIN} is absent or stale"
    mkdir -p "$(dirname "$BIN")" || fail_env "cannot create $(dirname "$BIN")"
    tmp="${BIN}.tmp.$$"
    if ! (cd "$REPO" && "$go_cmd" build -o "$tmp" ./cmd/cooldown-residue/); then
        rm -f "$tmp"
        fail_env "build failed in ${REPO} (see the go output above)"
    fi
    chmod +x "$tmp" && mv -f "$tmp" "$BIN" || fail_env "cannot install ${BIN}"
    log "built ${BIN} from ${REPO}"
fi

args=(-db "$DB" -fleet-toml "$FLEET_TOML")
if [ "$QUIET" = "1" ]; then
    args+=(-quiet)
fi

# The detector owns the verdict; this wrapper prints it verbatim and passes the
# exit code through so the timer's unit result is the fleet's actual state.
"$BIN" "${args[@]}"
rc=$?
if [ "$rc" -eq 1 ]; then
    log "residue detected — read-only report above; correct it with the operator run of fleet-cooldown-policy.py --apply"
fi
exit "$rc"
