#!/bin/sh
# quiesce-check.sh — S14 §5: prove no process holds the tree.
#
# Exit codes: 0 quiescent · 10 busy (pids printed) · 65 /proc unreadable.
set -eu

WT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --worktree) WT="$2"; shift 2 ;;
    *) echo "quiesce-check: unknown arg $1" >&2; exit 65 ;;
  esac
done
[ -n "$WT" ] || { echo "quiesce-check: --worktree required" >&2; exit 65; }
[ -d /proc ] || { echo "quiesce-check: /proc unreadable" >&2; exit 65; }
# /proc reports resolved paths; a symlinked workdir would never match otherwise.
WT=$(cd "$WT" 2>/dev/null && pwd -P) || { echo "quiesce-check: no such worktree" >&2; exit 65; }

SELF=$$
MYPID=$$
BUSY=""
for p in /proc/[0-9]*; do
  pid=${p#/proc/}
  [ "$pid" = "$MYPID" ] && continue
  [ "$pid" = "1" ] && continue
  # cwd inside the tree
  cwd=$(readlink "$p/cwd" 2>/dev/null || true)
  case "$cwd" in
    "$WT"|"$WT"/*) BUSY="$BUSY $pid:cwd"; continue ;;
  esac
  # an open file inside the tree (only readable for our own processes)
  if [ -d "$p/fd" ]; then
    for fd in "$p"/fd/*; do
      tgt=$(readlink "$fd" 2>/dev/null || true)
      case "$tgt" in
        "$WT"/*) BUSY="$BUSY $pid:fd"; break ;;
      esac
    done
  fi
done

if [ -n "$BUSY" ]; then
  echo "busy:$BUSY" >&2
  exit 10
fi
echo "quiescent=$WT"
exit 0
