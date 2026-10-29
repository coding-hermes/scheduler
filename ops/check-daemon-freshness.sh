#!/usr/bin/env bash
#
# check-daemon-freshness.sh — SCHED-GAP-148 daemon-freshness guard.
#
# Answers one operational question with an exit code: is the RUNNING
# schedulerd built from a commit that already contains the newest
# admission/scheduling change on this branch?
#
# Why this exists: operators could read /api/v1/health and see a version
# string, but that string could not be correlated to a commit on main, so
# "did the restart pick up the fix?" was answered by diffing binary
# timestamps by hand. That failed twice — a restart at 23:51 that loaded the
# pre-fix build while the fix landed at 23:59 (2026-09-17), and b57c80f
# (SCHED-GAP-155 ADMIT log + admission counters) being pushed while the live
# daemon kept serving a pre-fix dirty build hours later.
#
# Usage:
#   ops/check-daemon-freshness.sh [--status-url URL] [--against REV]
#
#   --status-url URL   scheduler status endpoint
#                      (default http://127.0.0.1:9090/api/v1/status)
#   --against REV      commit/tag/branch to measure against (default HEAD)
#
# Exit codes:
#   0 FRESH    live build_sha == reference, or live is a descendant of it
#   1 STALE    live predates the reference commit, or the two have diverged
#   2 UNKNOWN  no usable build_sha (daemon predates the build-identity
#              wiring, is down, or the URL is not a scheduler status
#              endpoint), or the reported sha is not a commit in this
#              checkout
#
# Operator knobs (environment variables):
#   MAX_STATUS_TIMEOUT  seconds for curl timeout on /api/v1/status
#                       (default 10; increase if lock-starvation causes
#                        transient 5xx/timeout while the daemon is alive)
#
# The reference is the newest commit reachable from --against that touches
# the admission/scheduling surface (internal/scheduler, or the migrations
# file). The comparison is an ancestor test, not a timestamp comparison:
# mtimes and uptimes lie (a daemon can be restarted from an older build),
# commit topology does not.
#
# stdout carries exactly one verdict line (FRESH:/STALE:/UNKNOWN:); every
# human-facing note goes to stderr so the verdict stays machine-parseable.

set -euo pipefail

# Defaults.
#
# The daemon listens on 9090 (docs/reference/flags.md; the systemd unit passes
# --listen 127.0.0.1:9090). The old default of 8642 pointed at a port nothing on
# this host serves, so a bare run could only ever answer UNKNOWN.
#
# Never seed these from positional "$1"/"$2": the flag loop below already parses
# `--status-url`/`--against`, and seeding made the FLAG itself the value (with
# `--status-url URL`, $2 = URL landed in AGAINST, so a documented invocation
# exited "UNKNOWN: --against (http://…) does not resolve"). Positional arguments
# are accepted only when they do not look like flags, for older callers.
STATUS_URL="http://127.0.0.1:9090/api/v1/status"
AGAINST="HEAD"
if [[ $# -gt 0 && ${1} != -* ]]; then STATUS_URL="$1"; shift; fi
if [[ $# -gt 0 && ${1} != -* ]]; then AGAINST="$1"; shift; fi
MAX_STATUS_TIMEOUT="${MAX_STATUS_TIMEOUT:-10}"

# Parse flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        --status-url)
            STATUS_URL="$2"
            shift 2
            ;;
        --against)
            AGAINST="$2"
            shift 2
            ;;
        --help|-h)
            cat <<EOF
Usage: $(basename "$0") [--status-url URL] [--against REV] [--help]

Operational guard: is the running schedulerd fresh (contains the newest
admission/scheduling change on --against)?

Flags:
  --status-url URL   scheduler status endpoint (default http://127.0.0.1:9090/api/v1/status)
  --against REV      commit/tag/branch to measure against (default HEAD)
  --help             show this help

Operator knobs (environment variables):
  MAX_STATUS_TIMEOUT  seconds for curl timeout on /api/v1/status
                      (default 10; increase if lock-starvation causes
                       transient timeouts while the daemon is alive)

Exit codes:
  0  FRESH    — live build_sha == reference, or live is a descendant of it
  1  STALE    — live predates the reference commit, or the two diverged
  2  UNKNOWN  — no usable build_sha (daemon is down, pre-build-identity,
               or the URL is not a scheduler status endpoint)

Examples:
  ops/check-daemon-freshness.sh
  ops/check-daemon-freshness.sh --against v1.12.0
  MAX_STATUS_TIMEOUT=30 ops/check-daemon-freshness.sh --status-url http://localhost:9090/api/v1/status
EOF
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            exit 1
            ;;
    esac
done

# Check daemon status
HTTP_BODY="$(curl -fsS --max-time "$MAX_STATUS_TIMEOUT" "$STATUS_URL" 2>/dev/null || true)"

if [ -z "$HTTP_BODY" ]; then
    echo "UNKNOWN: daemon down or wrong endpoint"
    exit 2
fi

LIVE_SHA=$(printf '%s' "$HTTP_BODY" | jq -r '.build_sha // empty' 2>/dev/null || true)

if [ -z "$LIVE_SHA" ]; then
    echo "UNKNOWN: daemon version string not machine-parseable (pre-build-identity?)"
    exit 2
fi

# A build_sha is only usable when it names a commit this checkout can resolve.
# A shape-only string ("unknown", "deadbeef") is not build identity: it can
# never be an ancestor of anything, so treating it as STALE would report a
# fresh daemon as garbage instead of admitting we do not know.
if ! LIVE_FULL=$(git rev-parse --verify --quiet "${LIVE_SHA}^{commit}" 2>/dev/null); then
    echo "UNKNOWN: reported build_sha ($LIVE_SHA) is not a commit in this checkout"
    exit 2
fi
LIVE_SHORT=$(git rev-parse --short=8 "$LIVE_FULL" 2>/dev/null || echo "$LIVE_SHA")

echo "LIVE SHA: $LIVE_SHA (reference: $AGAINST)" >&2

# The reference is the newest commit reachable from --against that touches
# the admission/scheduling surface (internal/scheduler, or the migrations
# file). The comparison is an ancestor test, not a timestamp comparison:
# mtimes and uptimes lie (a daemon can be restarted from an older build),
# commit topology does not.

# Find the reference commit (newest commit touching the admission/scheduling surface)
REFERENCE=$(git rev-list -n 1 "$AGAINST" -- internal/scheduler 2>/dev/null || true)
if [ -z "${REFERENCE:-}" ]; then
    REFERENCE=$(git rev-parse --verify --quiet "$AGAINST" 2>/dev/null || true)
fi
if [ -z "${REFERENCE:-}" ]; then
    echo "UNKNOWN: --against ($AGAINST) does not resolve in this checkout"
    exit 2
fi
REF_SHORT=$(git rev-parse --short=8 "$REFERENCE" 2>/dev/null || echo "$REFERENCE")

echo "REFERENCE: $REFERENCE (from --against=$AGAINST)" >&2

# Ancestor test: is the live build a descendant of (or equal to) the reference?
# i.e. is the reference already contained in the running build?
if git merge-base --is-ancestor "$REFERENCE" "$LIVE_FULL" 2>/dev/null; then
    echo "FRESH: live=$LIVE_SHORT reference=$REF_SHORT"
    exit 0
else
    echo "STALE: live=$LIVE_SHORT reference=$REF_SHORT"
    exit 1
fi
