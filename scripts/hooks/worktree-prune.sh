#!/bin/sh
# worktree-prune.sh — S14 §7: remove a lane worktree only when proved landed.
#
# Exit codes: 0 pruned or deliberately kept · 65 refused (state unexpected)
set -eu

LANE=""; REPO=""; WT=""; POLICY="persistent"; MERGED="0"
while [ $# -gt 0 ]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --worktree) WT="$2"; shift 2 ;;
    --policy) POLICY="$2"; shift 2 ;;
    --merged) MERGED="$2"; shift 2 ;;
    *) echo "worktree-prune: unknown arg $1" >&2; exit 65 ;;
  esac
done
[ -n "$LANE" ] && [ -n "$REPO" ] && [ -n "$WT" ] || { echo "worktree-prune: --lane --repo --worktree required" >&2; exit 65; }

BRANCH="lane/$(printf '%s' "$LANE" | tr '-' '/')"

if [ "$POLICY" != "ephemeral" ] && [ "$MERGED" != "1" ]; then
  echo "prune=kept reason=unmerged policy=$POLICY"
  exit 0
fi

if git -C "$REPO" worktree list --porcelain | grep -qx "worktree $WT"; then
  git -C "$REPO" worktree remove --force "$WT" || { echo "prune=refused reason=remove-failed" >&2; exit 65; }
fi

# -d refuses an unmerged branch by design: git is the second lock on "landed".
if [ "$MERGED" = "1" ]; then
  git -C "$REPO" branch -d "$BRANCH" >/dev/null 2>&1 || echo "prune=branch-kept reason=unmerged"
fi
echo "prune=removed worktree=$WT merged=$MERGED"
exit 0
