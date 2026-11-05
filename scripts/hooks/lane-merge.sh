#!/bin/sh
# lane-merge.sh — S14 §6.2: land a lane branch on the base branch, and PROVE it.
#
# Exit codes: 0 landed · 10 nothing to do · 65 blocked/conflict (work preserved)
#             75 retryable contention (push race, index.lock after retries)
set -eu

LANE=""; REPO=""; WT=""; BASE="main"; PUSH="0"
PROJECT=""; ROLE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --lane) LANE="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --worktree) WT="$2"; shift 2 ;;
    --base) BASE="$2"; shift 2 ;;
    --push) PUSH="$2"; shift 2 ;;
    --project) PROJECT="$2"; shift 2 ;;
    --role) ROLE="$2"; shift 2 ;;
    *) echo "lane-merge: unknown arg $1" >&2; exit 65 ;;
  esac
done
[ -n "$LANE" ] && [ -n "$REPO" ] && [ -n "$WT" ] || { echo "lane-merge: --lane --repo --worktree required" >&2; exit 65; }

die() { echo "lane-merge: $2" >&2; exit "$1"; }
# Same rule as worktree-ensure.sh, from the same file: lane/<project>/<role>.
. "$(dirname "$0")/lane-lib.sh"
BRANCH=$(lane_branch "$LANE" "$PROJECT" "$ROLE") \
  || die 65 "cannot derive a lane branch from lane=$LANE project=$PROJECT role=$ROLE"

[ -d "$WT" ] || die 65 "no worktree at $WT"
if [ -n "$(git -C "$WT" status --porcelain 2>/dev/null)" ]; then
  die 65 "dirty worktree — refusing to merge uncommitted work"
fi

AHEAD=$(git -C "$WT" rev-list --count "origin/$BASE..HEAD" 2>/dev/null || echo 0)
if [ "$AHEAD" -eq 0 ]; then
  echo "merge=no_commits ahead=0"
  exit 10
fi

git -C "$WT" fetch -q origin "$BASE" || die 75 "fetch failed"

BEHIND=$(git -C "$WT" rev-list --count "HEAD..origin/$BASE" 2>/dev/null || echo 0)
if [ "$BEHIND" -gt 0 ]; then
  if ! git -C "$WT" rebase "origin/$BASE" >/dev/null 2>&1; then
    git -C "$WT" rebase --abort >/dev/null 2>&1 || true
    echo "merge=conflict stage=rebase" >&2
    exit 65
  fi
fi

# Land in the source repository, never in the worktree — and never silently in
# the WRONG branch of it. `git merge --ff-only <branch>` advances whatever HEAD
# the repo happens to carry, so a checkout sitting on `master` (or on a
# published branch, which is how the code-factory workdir is registered) would
# either land the lane somewhere nobody asked for or refuse with a misleading
# "ff-only blocked". Measured: a fixture repo on master, base main, produced
# exactly that — the merge went to master and the next run could not fast-
# forward. Name the branch, refuse the mismatch, and leave the checkout alone.
CUR=$(git -C "$REPO" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")
if [ "$CUR" != "$BASE" ]; then
  if [ -n "$(git -C "$REPO" status --porcelain 2>/dev/null)" ]; then
    echo "merge=blocked stage=branch reason=repo-on-$CUR-not-$BASE-and-dirty" >&2
    exit 65
  fi
  if ! git -C "$REPO" checkout -q "$BASE" 2>/dev/null; then
    echo "merge=blocked stage=branch reason=cannot-checkout-$BASE-from-$CUR" >&2
    exit 65
  fi
fi

attempt=1
while : ; do
  if git -C "$REPO" merge --ff-only "$BRANCH" >/dev/null 2>&1; then
    break
  fi
  if [ -e "$REPO/.git/index.lock" ] && [ "$attempt" -lt 3 ]; then
    attempt=$((attempt+1)); sleep 2; continue
  fi
  echo "merge=blocked stage=ff-only branch=$CUR base=$BASE" >&2
  exit 65
done

if [ "$PUSH" = "1" ]; then
  if ! git -C "$REPO" push origin "$BASE" >/dev/null 2>&1; then
    echo "merge=local push=failed" >&2
    exit 75
  fi
fi

# Assert the merge instead of believing it.
LANDED_REF="$BASE"
if [ "$PUSH" = "1" ]; then LANDED_REF="origin/$BASE"; fi
git -C "$REPO" merge-base --is-ancestor "$BRANCH" "$LANDED_REF" || { echo "merge=unverified ancestry" >&2; exit 65; }

FILES=$(git -C "$WT" diff --name-only "origin/$BASE...$BRANCH" || true)
for f in $FILES; do
  git -C "$REPO" cat-file -e "$LANDED_REF:$f" 2>/dev/null || { echo "merge=unverified file $f" >&2; exit 65; }
done

SHA=$(git -C "$REPO" rev-parse --short "$LANDED_REF")
COUNT=$(printf '%s\n' "$FILES" | grep -c . || true)
echo "merge=landed sha=$SHA files=$COUNT pushed=$PUSH"
exit 0
