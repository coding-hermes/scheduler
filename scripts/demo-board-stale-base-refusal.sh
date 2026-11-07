#!/usr/bin/env bash
# demo-board-stale-base-refusal.sh — RELEASE-046 arm 4 demo.
#
# Builds a throwaway "origin" whose board carries two rows, clones a
# checkout that sits one commit BEHIND it (one row), stages a board edit
# from that stale base, and runs the REAL scripts/board-commit-guard.sh
# that sits next to this script. Exits 0 ONLY when the guard REFUSED the
# stale-base board commit and named the row it would have deleted (and, as
# a second beat, when BOARD_GUARD_ALLOW_ROW_DROP admits exactly that row,
# non-silently).
#
# Self-contained: everything happens under a mktemp dir; this repo's board,
# hooks, and config are never touched.
set -u

DEMO_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/board-stale-base-demo.XXXXXX") || exit 2
trap 'rm -rf "$DEMO_ROOT"' EXIT HUP INT TERM
HERE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) || exit 2
GUARD="$HERE/board-commit-guard.sh"
if [ ! -f "$GUARD" ]; then
    echo "demo: guard script missing next to $0" >&2
    exit 2
fi

export GIT_AUTHOR_NAME=demo GIT_AUTHOR_EMAIL=demo@example.com
export GIT_COMMITTER_NAME=demo GIT_COMMITTER_EMAIL=demo@example.com

ORIGIN="$DEMO_ROOT/origin"
CLONE="$DEMO_ROOT/clone"
BOARD_PATH=.coding-hermes/board/tasks.jsonl
ROW_ONE='{"id": "row-one", "status": "open"}'
ROW_TWO='{"id": "row-two", "status": "open"}'

# 1. Origin: main starts with ONE board row.
git init -q -b main "$ORIGIN"
mkdir -p "$ORIGIN/.coding-hermes/board"
printf '%s\n' "$ROW_ONE" > "$ORIGIN/$BOARD_PATH"
git -C "$ORIGIN" add -A
git -C "$ORIGIN" commit -q -m "board: row-one"

# 2. Clone while origin/main still carries ONE row, THEN advance origin to
#    two rows and FETCH in the clone. The fetch moves the clone's
#    origin/main remote-tracking ref (two rows) while its local branch sits
#    at one — the exact post-fetch, pre-rebase state the guard polices (the
#    guard compares against the locally-fetched origin/main; it does no
#    network I/O itself).
git clone -q "$ORIGIN" "$CLONE"
printf '%s\n' "$ROW_ONE" "$ROW_TWO" > "$ORIGIN/$BOARD_PATH"
git -C "$ORIGIN" add -A
git -C "$ORIGIN" commit -q -m "board: append row-two"
git -C "$CLONE" fetch -q origin

echo "origin/main board rows (fetched): row-one, row-two"
echo "clone local branch (stale base):  row-one only"

# 3. Stage the stale snapshot plus the writer's own new row — the exact
#    stale-base rewrite class the guard must refuse.
printf '%s\n' "$ROW_ONE" '{"id": "row-three", "status": "open"}' > "$CLONE/$BOARD_PATH"
git -C "$CLONE" add "$BOARD_PATH"

OUT=$(cd "$CLONE" && sh "$GUARD" 2>&1)
RC=$?

echo "---- guard output (exit $RC) ----"
printf '%s\n' "$OUT"
echo "---------------------------------"

if [ "$RC" -ne 1 ]; then
    echo "demo: FAIL — the guard did not refuse the stale-base board commit (rc=$RC)" >&2
    exit 1
fi
case "$OUT" in
    *row-two*) ;;
    *) echo "demo: FAIL — refusal does not name the dropped row (row-two)" >&2
       exit 1 ;;
esac

# 4. Second beat: the escape hatch admits exactly the named row, loudly.
OUT2=$(cd "$CLONE" && BOARD_GUARD_ALLOW_ROW_DROP=row-two sh "$GUARD" 2>&1)
RC2=$?
echo "---- guard output with BOARD_GUARD_ALLOW_ROW_DROP=row-two (exit $RC2) ----"
printf '%s\n' "$OUT2"
echo "--------------------------------------------------------------------------"

if [ "$RC2" -ne 0 ]; then
    echo "demo: FAIL — BOARD_GUARD_ALLOW_ROW_DROP=row-two did not admit the commit (rc=$RC2)" >&2
    exit 1
fi
case "$OUT2" in
    *NOTICE*) ;;
    *) echo "demo: FAIL — admitted drop did not print the NOTICE line" >&2
       exit 1 ;;
esac

echo "demo: PASS — stale-base board commit refused and named row-two; the escape hatch admitted it explicitly."
exit 0
