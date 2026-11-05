#!/bin/sh
# worktree-prune.sh — S14 §7: remove a lane worktree only when proved landed.
#
# Exit codes: 0 pruned or deliberately kept · 65 refused (state unexpected)
set -eu

LANE=""; REPO=""; WT=""; POLICY="persistent"; MERGED="0"; PROJECT=""; ROLE=""; BASE="main"
while [ $# -gt 0 ]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --worktree) WT="$2"; shift 2 ;;
    --policy) POLICY="$2"; shift 2 ;;
    --merged) MERGED="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --project) PROJECT="$2"; shift 2 ;;
    --role) ROLE="$2"; shift 2 ;;
    *) echo "worktree-prune: unknown arg $1" >&2; exit 65 ;;
  esac
done
[ -n "$LANE" ] && [ -n "$REPO" ] && [ -n "$WT" ] || { echo "worktree-prune: --lane --repo --worktree required" >&2; exit 65; }

# The lane -> branch rule is lane-lib.sh's (same file the other two scripts
# source): lane/<project>/<role>. This script spelled it with every dash turned
# into a slash, so it looked for lane/code/factory/qa, found nothing to prove
# landed, and either kept the tree or refused — the third copy of one wrong
# rule, and the one a POST chain hits last, where a failure is least noticed.
if [ -f "$(dirname "$0")/lane-lib.sh" ]; then
  . "$(dirname "$0")/lane-lib.sh"
  BRANCH=$(lane_branch "$LANE" "$PROJECT" "$ROLE") || { echo "worktree-prune: cannot derive a lane branch from lane=$LANE" >&2; exit 65; }
else
  BRANCH="lane/$LANE"
fi

if [ "$POLICY" != "ephemeral" ] && [ "$MERGED" != "1" ]; then
  echo "prune=kept reason=unmerged policy=$POLICY"
  exit 0
fi

removed="0"
if git -C "$REPO" worktree list --porcelain | grep -qx "worktree $WT"; then
  git -C "$REPO" worktree remove --force "$WT" || { echo "prune=refused reason=remove-failed" >&2; exit 65; }
  removed="1"
fi

# git is the second lock on "landed" — but `branch -d` compares against the
# branch's UPSTREAM (origin/<base>), not against the base branch itself. With
# push=0 the lane is merged to local main and NOT yet on origin/main, so -d
# refuses with "not yet merged to refs/remotes/origin/main, even though it is
# merged to HEAD". Reporting that as "reason=unmerged" is a lie in the one place
# an operator looks to decide whether the work landed. Prove ancestry against
# the base ref we were told to land on, then delete with -D on the strength of
# that proof; otherwise keep the branch and say exactly which ref it is missing
# from.
if [ "$MERGED" = "1" ]; then
  if git -C "$REPO" merge-base --is-ancestor "$BRANCH" "$BASE" 2>/dev/null; then
    if git -C "$REPO" branch -D "$BRANCH" >/dev/null 2>&1; then
      echo "prune=branch-removed branch=$BRANCH proven-merged-to=$BASE"
    else
      echo "prune=branch-kept reason=delete-failed branch=$BRANCH"
    fi
  else
    echo "prune=branch-kept reason=not-an-ancestor-of-$BASE branch=$BRANCH"
  fi
fi
# Say what happened. Reporting "removed" for a path that was never there is the
# same class of lie as a gate that prints PASS for a suite it skipped: the next
# reader believes a tree was cleaned up and stops looking for it.
if [ "$removed" = "1" ]; then
  echo "prune=removed worktree=$WT merged=$MERGED"
else
  echo "prune=nothing-to-remove worktree=$WT merged=$MERGED"
fi
exit 0
