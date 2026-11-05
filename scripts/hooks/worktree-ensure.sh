#!/bin/sh
# worktree-ensure.sh — S14 §4: create or refresh a lane worktree.
#
# Exit codes (the hook contract):
#   0  ok (created or refreshed; unchanged is also 0)
#   65 blocked/quarantined (unexpected repo or worktree state)
#   75 retryable contention (branch checked out elsewhere, stale index.lock)
#   78 precondition unmet (disk floor, not a repo, no origin/<base>)
set -eu

LANE=""; REPO=""; WT=""; BASE="main"; POLICY="persistent"; FLOOR_GIB="5"
while [ $# -gt 0 ]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --worktree) WT="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --policy) POLICY="$2"; shift 2 ;;
    --floor-gib) FLOOR_GIB="$2"; shift 2 ;;
    *) echo "worktree-ensure: unknown arg $1" >&2; exit 65 ;;
  esac
done
[ -n "$LANE" ] && [ -n "$REPO" ] && [ -n "$WT" ] || { echo "worktree-ensure: --lane --repo --worktree required" >&2; exit 65; }

die() { echo "worktree-ensure: $2" >&2; exit "$1"; }

BRANCH="lane/$(printf '%s' "$LANE" | tr '-' '/')"

# 1. repo sanity
[ -d "$REPO/.git" ] || die 78 "not a git work tree: $REPO"
git -C "$REPO" rev-parse --verify -q "origin/$BASE" >/dev/null 2>&1 || die 78 "no origin/$BASE in $REPO"
if [ -f "$REPO/.gitmodules" ]; then die 65 "submodules are not supported by S14 (see spec §9)"; fi

# 2. disk floor
PARENT=$(dirname "$WT")
mkdir -p "$PARENT"
AVAIL=$(df -Pk "$PARENT" | awk 'NR==2{printf "%.1f", $4/1048576}')
if awk -v a="$AVAIL" -v f="$FLOOR_GIB" 'BEGIN{exit !(a<f)}'; then
  die 78 "disk floor: ${AVAIL}GiB free < ${FLOOR_GIB}GiB"
fi

# 3. path state
if [ -e "$WT" ] && ! git -C "$REPO" worktree list --porcelain | grep -qx "worktree $WT"; then
  if [ -d "$WT" ] && [ -z "$(ls -A "$WT" 2>/dev/null)" ]; then
    rmdir "$WT" || die 65 "cannot remove empty non-worktree dir $WT"
    git -C "$REPO" worktree prune
  else
    die 65 "path exists but is not a registered worktree and is not empty: $WT"
  fi
fi

# 4. branch must not be checked out in another worktree
OWNER=$(git -C "$REPO" worktree list --porcelain \
  | awk -v b="branch refs/heads/$BRANCH" -v want="$WT" '
      /^worktree /{w=$2}
      $0=="branch refs/heads/'"$BRANCH"'"{ if (w != want) print w }')
if [ -n "$OWNER" ]; then die 75 "branch $BRANCH is checked out at $OWNER"; fi

if [ ! -e "$WT" ]; then
  git -C "$REPO" worktree add -B "$BRANCH" -- "$WT" "origin/$BASE" >/dev/null 2>&1 \
    || { git -C "$REPO" worktree prune; git -C "$REPO" worktree add -B "$BRANCH" -- "$WT" "origin/$BASE" >/dev/null 2>&1 || die 65 "worktree add failed"; }
fi

CUR_BRANCH=$(git -C "$WT" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")
if [ "$CUR_BRANCH" != "$BRANCH" ]; then die 65 "worktree at $WT is on '$CUR_BRANCH', expected '$BRANCH'"; fi

# 5. refresh
DIRTY=0
if [ -n "$(git -C "$WT" status --porcelain 2>/dev/null)" ]; then DIRTY=1; fi

case "$POLICY" in
  ephemeral)
    git -C "$WT" fetch -q origin "$BASE" || die 75 "fetch failed"
    git -C "$WT" reset --hard "origin/$BASE" >/dev/null
    git -C "$WT" clean -fdq -e .venv -e node_modules -e target
    DIRTY=0
    ;;
  persistent-rebase)
    if [ "$DIRTY" -eq 0 ]; then
      git -C "$WT" fetch -q origin "$BASE" || die 75 "fetch failed"
      if ! git -C "$WT" rebase "origin/$BASE" >/dev/null 2>&1; then
        git -C "$WT" rebase --abort >/dev/null 2>&1 || true
        die 65 "rebase onto origin/$BASE conflicted"
      fi
    fi
    ;;
  persistent)
    if [ "$DIRTY" -eq 0 ]; then
      git -C "$WT" fetch -q origin "$BASE" || die 75 "fetch failed"
      git -C "$WT" reset --hard "origin/$BASE" >/dev/null
      git -C "$WT" clean -fdq -e .venv -e node_modules -e target
    fi
    ;;
  *) die 65 "unknown policy $POLICY" ;;
esac

BASE_SHA=$(git -C "$WT" rev-parse --short "origin/$BASE")
AHEAD=$(git -C "$WT" rev-list --count "origin/$BASE..HEAD" 2>/dev/null || echo 0)
if [ "$DIRTY" -eq 1 ]; then echo "dirty_preserved"; fi
echo "worktree=$WT branch=$BRANCH base=$BASE_SHA ahead=$AHEAD dirty=$DIRTY"
exit 0
