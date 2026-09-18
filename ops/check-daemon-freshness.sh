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
# The reference is the newest commit reachable from --against that touches
# the admission/scheduling surface (internal/scheduler, or the migrations
# file). The comparison is an ancestor test, not a timestamp comparison:
# mtimes and uptimes lie (a daemon can be restarted from an older build),
# commit topology does not.
#
# stdout carries exactly one verdict line (FRESH:/STALE:/UNKNOWN:); every
# human-facing note goes to stderr so the verdict stays machine-parseable.

set -euo pipefail

DEFAULT_STATUS_URL="http://127.0.0.1:9090/api/v1/status"

# The admission/scheduling surface: the newest commit touching any of these
# paths is the newest change that can alter what the scheduler admits.
SCHED_PATHS=(internal/scheduler internal/database/migrations.go)

usage() {
	cat <<'USAGE'
usage: check-daemon-freshness.sh [--status-url URL] [--against REV]

  --status-url URL  scheduler status endpoint
                    (default http://127.0.0.1:9090/api/v1/status)
  --against REV     commit/tag/branch the running build is measured against
                    (default HEAD)

exit 0 FRESH   live build_sha == reference, or live is a descendant of it
exit 1 STALE   live predates the reference commit, or the two diverged
exit 2 UNKNOWN no usable build_sha from the daemon (or unresolvable shas)

Prints one verdict line to stdout; notes go to stderr.
USAGE
}

note() { printf '%s\n' "$*" >&2; }
verdict() { printf '%s\n' "$*"; }

# short_sha resolves any revision to git's 8-char abbreviation (matching
# internal/version.CurrentCommit's shape), falling back to the input so a
# verdict line always prints something comparable.
short_sha() {
	local s=""
	s="$(git rev-parse --short=8 "$1" 2>/dev/null || true)"
	if [ -z "$s" ]; then
		printf '%s' "$1"
	else
		printf '%s' "$s"
	fi
}

# extract_build_sha pulls .build_sha out of the status body. jq is the
# documented path; the sed fallback keeps the guard usable on hosts without
# jq (it prints nothing when the key is absent, which classifies as UNKNOWN).
extract_build_sha() {
	if command -v jq >/dev/null 2>&1; then
		jq -r '.build_sha // empty' 2>/dev/null || true
	else
		sed -n 's/.*"build_sha"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1
	fi
}

STATUS_URL="$DEFAULT_STATUS_URL"
AGAINST="HEAD"

while [ $# -gt 0 ]; do
	case "$1" in
	--status-url)
		STATUS_URL="${2:-}"
		[ -n "$STATUS_URL" ] || {
			note "check-daemon-freshness: --status-url needs a URL"
			exit 2
		}
		shift 2
		;;
	--status-url=*)
		STATUS_URL="${1#*=}"
		shift
		;;
	--against)
		AGAINST="${2:-}"
		[ -n "$AGAINST" ] || {
			note "check-daemon-freshness: --against needs a revision"
			exit 2
		}
		shift 2
		;;
	--against=*)
		AGAINST="${1#*=}"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		note "check-daemon-freshness: unknown argument '$1'"
		usage >&2
		exit 2
		;;
	esac
done

for tool in git curl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		note "check-daemon-freshness: required tool '$tool' not found in PATH"
		exit 2
	fi
done

STATUS_HOST="${STATUS_URL#*://}"
STATUS_HOST="${STATUS_HOST%%/*}"
DAEMON="schedulerd@${STATUS_HOST}"

# Reference: newest admission/scheduling-touching commit reachable from REV.
RESOLVED_REV="$(git rev-parse --verify --quiet "${AGAINST}^{commit}" || true)"
if [ -z "$RESOLVED_REV" ]; then
	note "check-daemon-freshness: cannot resolve --against '${AGAINST}' to a commit — run this from the scheduler checkout"
	verdict "UNKNOWN: live=<not checked> reference=${AGAINST} (unresolvable revision)"
	exit 2
fi

REF_SHA="$(git rev-list -1 "$RESOLVED_REV" -- "${SCHED_PATHS[@]}" 2>/dev/null || true)"
if [ -z "$REF_SHA" ]; then
	# No reachable commit touches the admission surface (fresh/shallow
	# checkout): fall back to the resolved revision itself rather than
	# silently reporting FRESH off an empty reference.
	REF_SHA="$RESOLVED_REV"
	note "check-daemon-freshness: no history touching ${SCHED_PATHS[*]} reachable from ${AGAINST}; using ${AGAINST} itself as the reference"
fi

# Live: what the running daemon says it was built from.
HTTP_BODY="$(curl -fsS --max-time 10 "$STATUS_URL" 2>/dev/null || true)"
if [ -z "$HTTP_BODY" ]; then
	note "check-daemon-freshness: no JSON response from ${STATUS_URL} — daemon down or wrong endpoint"
	verdict "UNKNOWN: live=<no response> reference=$(short_sha "$REF_SHA") (${DAEMON}; curled ${STATUS_URL})"
	exit 2
fi

LIVE_SHA="$(printf '%s' "$HTTP_BODY" | extract_build_sha || true)"
LIVE_SHA="$(printf '%s' "$LIVE_SHA" | tr -d '[:space:]')"

case "$LIVE_SHA" in
"" | unknown | null | "<nil>")
	note "check-daemon-freshness: ${DAEMON} reports no usable build_sha (got '${LIVE_SHA:-<missing>}') at ${STATUS_URL}"
	note "  a daemon predating the build-identity wiring cannot answer freshness questions — rebuild and restart it"
	verdict "UNKNOWN: live=${LIVE_SHA:-<missing>} reference=$(short_sha "$REF_SHA") (${DAEMON}; no usable build_sha at ${STATUS_URL})"
	exit 2
	;;
esac

LIVE_FULL="$(git rev-parse --verify --quiet "${LIVE_SHA}^{commit}" || true)"
if [ -z "$LIVE_FULL" ]; then
	note "check-daemon-freshness: reported build_sha '${LIVE_SHA}' is not a commit in this checkout (ambiguous abbreviation, or the daemon was built from another clone)"
	verdict "UNKNOWN: live=${LIVE_SHA} reference=$(short_sha "$REF_SHA") (${DAEMON}; sha not resolvable in this checkout)"
	exit 2
fi

LIVE_SHORT="$(short_sha "$LIVE_FULL")"
REF_SHORT="$(short_sha "$REF_SHA")"

if [ "$LIVE_FULL" = "$REF_SHA" ]; then
	verdict "FRESH: live=${LIVE_SHORT} reference=${REF_SHORT}"
	exit 0
fi

if git merge-base --is-ancestor "$REF_SHA" "$LIVE_FULL" 2>/dev/null; then
	note "check-daemon-freshness: live ${LIVE_SHORT} descends from reference ${REF_SHORT} — the daemon is at or after the newest admission change"
	verdict "FRESH: live=${LIVE_SHORT} reference=${REF_SHORT}"
	exit 0
fi

if git merge-base --is-ancestor "$LIVE_FULL" "$REF_SHA" 2>/dev/null; then
	note "check-daemon-freshness: live ${LIVE_SHORT} is an ANCESTOR of reference ${REF_SHORT} — the daemon was built before that change landed (restart from a rebuilt binary)"
else
	note "check-daemon-freshness: live ${LIVE_SHORT} and reference ${REF_SHORT} have DIVERGED — the running binary is not on this branch"
fi
verdict "STALE: live=${LIVE_SHORT} reference=${REF_SHORT}"
exit 1
