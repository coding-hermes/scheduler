#!/bin/sh
# board-commit-guard.sh — SCHED-GAP-1699 (P0) + RELEASE-046 stale-base rule.
#
# Rule 1 (SCHED-GAP-1699, board-mixing): a CODE row commit must never touch
# .coding-hermes/board/*. Workers run in per-task worktrees whose
# .coding-hermes/board/tasks.jsonl is a STALE snapshot of the board; when a
# worker board-writes and commits code, the merge diff is a full-file rewrite
# that DELETES every row appended since the branch (three incidents:
# REMOTE-006 550-row rewrite; REMOTE-008 deleted SCHED-GAP-1695/1696). This
# guard refuses any changeset that mixes board paths with non-board paths. A
# commit whose EVERY changed path is under .coding-hermes/ (board-only
# truthkeeping) passes, and a pure code commit passes.
#
# Rule 2 (RELEASE-046, stale-base row drop): a board-ONLY commit is REFUSED
# when the staged (index) blob of a board file would DROP any row id that the
# same path carries on origin/main (origin/master fallback). This is the
# canonical-checkout variant of the same incident class: a board commit made
# from a base BEHIND origin/main stages the whole file from that base and
# silently deletes every row appended since the branch (on 2026-11-07 the
# canonical checkout sat 6 commits behind carrying a row; RELEASE-043/044/045
# would have been deleted). A legitimate edit still passes — the rule is "no
# id may disappear", not "the file may not change": status flips and row
# appends are fine, a dropped id is not.
#
# Escape hatch (explicit, non-silent): BOARD_GUARD_ALLOW_ROW_DROP may hold a
# comma-separated list of row ids; a drop is admitted ONLY when EVERY dropped
# id is named in it, and every admission prints a NOTICE line to stderr. Any
# unlisted dropped id still refuses. There is no silent override.
#
# The row-id extraction and comparison live in
# scripts/board_stale_base_check.py (next to this script); the guard invokes
# it only when a board path is staged. Row identity is extracted generically
# per line: {"id": "<value>"} for tasks.jsonl (string ids) and {"id": <value>}
# for events.jsonl (integer ids); lines that do not parse are skipped, never
# fatal.
#
# Path source (in order):
#   1. arguments  — each $n is one changed path (test/CLI convenience)
#   2. stdin      — one path per line (hook tests, pipes)
#   3. git        — `git diff --cached --name-only` when stdin is empty and
#                   the cwd is a git work tree (the real pre-commit case)
# Empty stdin + no git repo is fail-closed (exit 2): an unverifiable
# changeset is refused, never waved through. The stale-base rule runs only
# when a board path is STAGED in the index; a listed board path in a
# non-repo cwd has no index to compare against and is skipped (rule 1 still
# applies to it).
#
# Exit codes: 0 = allowed (including a BOARD_GUARD_ALLOW_ROW_DROP-admitted
# drop), 1 = refused (board-mixing changeset or stale-base row drop), 2 =
# could not determine or verify the changeset (fail-closed: no changed-path
# source, git failure, no origin ref, unreadable index blob, missing
# python3/helper, helper failure).

set -u

BOARD=0
OUTSIDE=0
BOARD_LIST=""
OUTSIDE_LIST=""
BOARD_PATHS=""
STAGED_BOARD=""
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" 2>/dev/null && pwd) || HERE=""

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
            BOARD_PATHS="$BOARD_PATHS$p
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

# ---- RELEASE-046: stale-base row-drop check (board-only commits) ----
# Reached only when the changeset is board-only. Runs when a board path is
# STAGED in the index (the commit-time case); a listed-but-not-staged board
# path in a non-repo cwd has no index to compare against and is skipped.
if [ "$BOARD" -gt 0 ] && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    STAGED_ALL=$(git diff --cached --name-only 2>/dev/null) || {
        printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): board path(s) listed but git diff --cached failed — refusing (fail-closed)" >&2
        exit 2
    }
    while IFS= read -r sp; do
        [ -n "$sp" ] || continue
        if printf '%s\n' "$BOARD_PATHS" | grep -Fxq -- "$sp"; then
            STAGED_BOARD="$STAGED_BOARD$sp
"
        fi
    done <<STAGED_ALL_HEREDOC
$STAGED_ALL
STAGED_ALL_HEREDOC
fi

if [ -n "$STAGED_BOARD" ]; then
    if git rev-parse --verify --quiet "origin/main^{commit}" >/dev/null 2>&1; then
        BASE_REF=origin/main
    elif git rev-parse --verify --quiet "origin/master^{commit}" >/dev/null 2>&1; then
        BASE_REF=origin/master
    else
        printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): board path(s) staged but neither origin/main nor origin/master exists to compare against — refusing (fail-closed)" >&2
        exit 2
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): board path(s) staged but python3 is unavailable for the row-id check — refusing (fail-closed)" >&2
        exit 2
    fi
    CHECK_HELPER="$HERE/board_stale_base_check.py"
    if [ -z "$HERE" ] || [ ! -f "$CHECK_HELPER" ]; then
        printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): row-id helper not found at ${CHECK_HELPER:-<unresolved script dir>} — refusing (fail-closed)" >&2
        exit 2
    fi
    CHECK_TMP=$(mktemp -d "${TMPDIR:-/tmp}/board-guard.XXXXXX") || {
        printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): mktemp failed — refusing (fail-closed)" >&2
        exit 2
    }
    trap 'rm -rf "$CHECK_TMP"' EXIT HUP INT TERM
    # The loop reads its path list on fd 3 so git/python3 children in the
    # body cannot steal the heredoc stdin.
    while IFS= read -r p <&3; do
        [ -n "$p" ] || continue
        # Staged (index) content; a staged DELETION reads as empty, so every
        # row the base carries counts as dropped — deleting the board file is
        # the maximal row drop and must refuse.
        if [ -n "$(git ls-files --stage -- "$p" 2>/dev/null)" ]; then
            git show ":$p" >"$CHECK_TMP/staged" 2>/dev/null || {
                printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): cannot read the index blob for $p — refusing (fail-closed)" >&2
                exit 2
            }
        else
            : >"$CHECK_TMP/staged"
        fi
        if ! BASE_OID=$(git ls-tree "$BASE_REF" -- "$p" 2>/dev/null | awk '{print $3}'); then
            printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): git ls-tree $BASE_REF failed for $p — refusing (fail-closed)" >&2
            exit 2
        fi
        if [ -n "$BASE_OID" ]; then
            git cat-file blob "$BASE_OID" >"$CHECK_TMP/base" 2>/dev/null || {
                printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): cannot read the $BASE_REF blob for $p — refusing (fail-closed)" >&2
                exit 2
            }
        else
            : >"$CHECK_TMP/base" # path absent on the base — nothing to drop
        fi
        HELPER_OUT=$(python3 "$CHECK_HELPER" "$CHECK_TMP/staged" "$CHECK_TMP/base" 2>"$CHECK_TMP/helper.err")
        HELPER_RC=$?
        if [ "$HELPER_RC" -ne 0 ] && [ "$HELPER_RC" -ne 1 ]; then
            {
                printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): row-id check failed on $p (helper rc=$HELPER_RC) — refusing (fail-closed)"
                cat "$CHECK_TMP/helper.err"
            } >&2
            exit 2
        fi
        if [ "$HELPER_RC" -eq 0 ]; then
            continue
        fi
        DROPPED="$HELPER_OUT"
        DROPPED_COUNT=0
        while IFS= read -r l; do
            [ -n "$l" ] || continue
            DROPPED_COUNT=$((DROPPED_COUNT + 1))
        done <<DROPPED_HEREDOC
$DROPPED
DROPPED_HEREDOC
        # Escape hatch: every dropped id must be named in
        # BOARD_GUARD_ALLOW_ROW_DROP (comma-separated). Non-silent: an
        # admitted drop prints a NOTICE line.
        ALLOW="${BOARD_GUARD_ALLOW_ROW_DROP:-}"
        ALL_ALLOWED=1
        while IFS= read -r did; do
            [ -n "$did" ] || continue
            found=0
            OLDIFS=$IFS
            IFS=,
            for a in $ALLOW; do
                a=$(printf '%s' "$a" | tr -d ' \t')
                if [ "$a" = "$did" ]; then
                    found=1
                fi
            done
            IFS=$OLDIFS
            if [ "$found" -ne 1 ]; then
                ALL_ALLOWED=0
            fi
        done <<ALLOW_HEREDOC
$DROPPED
ALLOW_HEREDOC
        if [ "$ALL_ALLOWED" -eq 1 ]; then
            printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): NOTICE — dropping $DROPPED_COUNT row id(s) from $p is explicitly allowed by BOARD_GUARD_ALLOW_ROW_DROP: $(printf '%s' "$DROPPED" | tr '\n' ' ')" >&2
            continue
        fi
        {
            printf '%s\n' "board-commit-guard (RELEASE-046 stale-base): REFUSED — the staged blob of $p drops $DROPPED_COUNT row id(s) still present on $BASE_REF."
            printf '%s\n' "Committing this index blob would DELETE those rows from the shared board (the stale-base rewrite class: REMOTE-006, REMOTE-008, RELEASE-046)."
            printf '%s\n' "Dropped id(s) (up to 10 shown):"
            shown=0
            while IFS= read -r did; do
                [ -n "$did" ] || continue
                shown=$((shown + 1))
                if [ "$shown" -gt 10 ]; then
                    printf '%s\n' "  ... and $((DROPPED_COUNT - 10)) more"
                    break
                fi
                printf '%s\n' "  $did"
            done <<DROPPED_LIST_HEREDOC
$DROPPED
DROPPED_LIST_HEREDOC
            printf '%s\n' "Remedy: git fetch origin && git rebase origin/main — then re-apply your row on top; or replay the dropped row(s) onto the fetched blob before committing."
            printf '%s\n' "Escape hatch (audited, non-silent): BOARD_GUARD_ALLOW_ROW_DROP=<comma-separated ids> admits ONLY the ids it names."
        } >&2
        exit 1
    done 3<<STAGED_BOARD_HEREDOC
$STAGED_BOARD
STAGED_BOARD_HEREDOC
fi

exit 0
