#!/bin/sh
# quiesce-check.sh — S14 §5: prove no process holds the tree.
#
# Exit codes: 0 quiescent · 11 busy (pids printed) · 65 /proc unreadable.
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

MYPID=$$
SCAN=$(mktemp "${TMPDIR:-/tmp}/quiesce-check.XXXXXX")
trap 'rm -f "$SCAN"' EXIT
# One find(1) resolves every link in-process. Printing the targets and doing the
# prefix match in shell avoids -lname interpreting glob characters in WT.
find /proc/[0-9]* -maxdepth 2 \
  \( -name cwd -o -path '*/fd/*' \) -type l -printf '%p %l\n' \
  >"$SCAN" 2>/dev/null &
FINDPID=$!
# Processes and fds disappear during a live /proc scan; that is not a scan error.
wait "$FINDPID" || true

BUSY=""
while IFS=' ' read -r link target; do
  pid=${link#/proc/}
  pid=${pid%%/*}
  [ "$pid" = "$MYPID" ] && continue
  [ "$pid" = "$FINDPID" ] && continue
  [ "$pid" = "1" ] && continue
  kind=""
  case "$link" in
    */cwd)
      case "$target" in "$WT"|"$WT"/*) kind=cwd ;; esac
      ;;
    */fd/*)
      case "$target" in "$WT"/*) kind=fd ;; esac
      ;;
  esac
  [ -n "$kind" ] || continue
  case "$BUSY" in
    *" $pid:"*) ;;
    *) BUSY="$BUSY $pid:$kind" ;;
  esac
done < "$SCAN"

if [ -n "$BUSY" ]; then
  echo "busy:$BUSY" >&2
  exit 11
fi
echo "quiescent=$WT"
exit 0
