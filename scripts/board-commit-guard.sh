#!/bin/sh
# board-commit-guard.sh — SCHED-GAP-1699 (P0).
#
# A CODE row commit must never touch .coding-hermes/board/*. Workers run in
# per-task worktrees whose .coding-hermes/board/tasks.jsonl is a STALE
# snapshot of the board; when a worker board-writes and commits code, the
# merge diff is a full-file rewrite that DELETES every row appended since
# the branch (three incidents: REMOTE-006 550-row rewrite; REMOTE-008
# deleted SCHED-GAP-1695/1696). This guard refuses any changeset that mixes
# board paths with non-board paths. A commit whose EVERY changed path is
# under .coding-hermes/ (board-only truthkeeping) passes, and a pure code
# commit passes.
#
# Path source (in order):
#   1. arguments  — each $n is one changed path (test/CLI convenience)
#   2. stdin      — one path per line (hook tests, pipes)
#   3. git        — `git diff --cached --name-only` when stdin is empty and
#                   the cwd is a git work tree (the real pre-commit case)
# Empty stdin + no git repo is fail-closed (exit 2): an unverifiable
# changeset is refused, never waved through.
#
# Exit codes: 0 = allowed, 1 = board-mixing changeset refused, 2 = could
# not determine the changeset (fail-closed).

set -u

BOARD=0
OUTSIDE=0
BOARD_LIST=""
OUTSIDE_LIST=""

# classify counts one changed path into the board bucket (under
# .coding-hermes/board/), the code bucket (anywhere outside
# .coding-hermes/), or neither (other .coding-hermes/ content — waves,
# config — is neither board truth nor code).
classify() {
    p=$1
    case $p in
        .coding-hermes/board/*)
            BOARD=$((BOARD + 1))
            BOARD_LIST="$BOARD_LIST  $p
"
            ;;
        .coding-hermes/*)
            : # .coding-hermes/ but not board/ — not a board rewrite
            ;;
        *)
            OUTSIDE=$((OUTSIDE + 1))
            OUTSIDE_LIST="$OUTSIDE_LIST  $p
"
            ;;
    esac
}

if [ "$#" -gt 0 ]; then
    for p in "$@"; do
        classify "$p"
    done
else
    GOT_STDIN=0
    while IFS= read -r p || [ -n "$p" ]; do
        [ -n "$p" ] || continue
        GOT_STDIN=1
        classify "$p"
    done
    if [ "$GOT_STDIN" -eq 0 ]; then
        if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
            printf '%s\n' "board-commit-guard (SCHED-GAP-1699): no changed-path source (empty stdin, not a git work tree) — refusing (fail-closed)" >&2
            exit 2
        fi
        STAGED=$(git diff --cached --name-only 2>/dev/null) || {
            printf '%s\n' "board-commit-guard (SCHED-GAP-1699): git diff --cached failed — refusing (fail-closed)" >&2
            exit 2
        }
        while IFS= read -r p; do
            [ -n "$p" ] || continue
            classify "$p"
        done <<EOF
$STAGED
EOF
    fi
fi

if [ "$BOARD" -gt 0 ] && [ "$OUTSIDE" -gt 0 ]; then
    {
        printf '%s\n' "board-commit-guard (SCHED-GAP-1699): REFUSED — this commit mixes .coding-hermes/board/ writes with code changes."
        printf '%s\n' "A worker worktree's board is a stale snapshot; committing it with code rewrites the board file and deletes every row appended since the branch (incidents REMOTE-006, REMOTE-008)."
        printf '%s\n' "Split the changeset: a commit is either board-only (every path under .coding-hermes/) or code-only (no path under .coding-hermes/board/)."
        printf '%s\n' "Board paths ($BOARD):"
        printf '%s' "$BOARD_LIST"
        printf '%s\n' "Code paths ($OUTSIDE):"
        printf '%s' "$OUTSIDE_LIST"
    } >&2
    exit 1
fi

exit 0
