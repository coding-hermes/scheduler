#!/bin/bash
# staging-reaper.sh — MON-GAP-031 (reconstruction 2026-11-10)
#
# Reap backup staging debris left behind by SIGKILLed backup-tiered*.sh /
# backup-remote.sh runs (orphaned `<stamp>/<db>` copies and verify_*.db files
# sitting in /var/tmp, i.e. on the fast disk, eating free space with no owner).
#
# Contract, as documented by the consumer that relies on it
# (monitoring/scripts/disk-guard.py, "Backup staging debris ... is deliberately
# NOT here — that is staging-reaper.sh's job (MON-GAP-031), whose proven-dead
# rule (producing pid dead + no live holder) cannot be expressed as an age
# glob"):
#
#   disk-guard must NOT sweep this pool, and this reaper must be FAIL-CLOSED.
#   Every one of these must hold before a single file is removed:
#     1. no backup producer process is alive
#        (backup-tiered.sh / backup-tiered-v3.sh / backup-remote.sh),
#     2. no process holds an open fd anywhere under the stage root,
#     3. the entry's mtime is older than MIN_AGE_H (default 6h).
#   The lock file is never removed. When in doubt: skip, log, exit 0.
#
# Usage: staging-reaper.sh [--dry-run]
# Exit: 0 = success (reaped, or deliberately skipped); 1 = stage root unusable.
set -uo pipefail

STAGE_ROOT="${STAGE_ROOT:-/var/tmp/hermes-backup-stage}"
MIN_AGE_H="${MIN_AGE_H:-6}"
LOG="${LOG:-$HOME/.hermes/logs/staging-reaper.log}"
PRODUCER_PATTERN='backup-tiered(-v3)?\.sh|backup-remote\.sh'

DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

mkdir -p "$(dirname "$LOG")" 2>/dev/null
log() { printf '%s staging-reaper: %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" | tee -a "$LOG"; }

[ -d "$STAGE_ROOT" ] || { log "SKIP stage root absent: $STAGE_ROOT"; exit 0; }

# Rule 1 — a live producer means the pool is in use; never touch it.
producers=$(pgrep -af "$PRODUCER_PATTERN" 2>/dev/null | grep -v staging-reaper)
if [ -n "$producers" ]; then
  log "SKIP live backup producer in progress: $(printf '%s' "$producers" | awk '{print $1}' | paste -sd,) — $(printf '%s' "$producers" | head -1 | cut -c1-160)"
  exit 0
fi

# Rule 2 — an open fd under the stage root means a process still holds a file
# there (sqlite3 .dump, scp mid-transfer, a verify pass). Never touch it.
holders=""
for fd in /proc/[0-9]*/fd/*; do
  tgt=$(readlink "$fd" 2>/dev/null) || continue
  case "$tgt" in
    "$STAGE_ROOT"/*) holders="$holders $(printf '%s' "$fd" | cut -d/ -f3)";;
  esac
done
if [ -n "$holders" ]; then
  log "SKIP live fd holder(s) under $STAGE_ROOT:$(printf '%s' "$holders" | tr ' ' '\n' | sort -u | paste -sd,) (deleted-or-open staging files)"
  exit 0
fi

# Rule 3 — age floor keeps anything a just-started run could be about to use.
candidates=$(find "$STAGE_ROOT" -mindepth 1 -maxdepth 1 ! -name '.backup.lock' -mmin +$((MIN_AGE_H * 60)) 2>/dev/null)
if [ -z "$candidates" ]; then
  log "OK nothing to reap: no entry older than ${MIN_AGE_H}h in $STAGE_ROOT ($(du -sh "$STAGE_ROOT" 2>/dev/null | cut -f1) held, no live holder)"
  exit 0
fi

reaped=0
while IFS= read -r entry; do
  [ -n "$entry" ] || continue
  sz=$(du -sh "$entry" 2>/dev/null | cut -f1)
  if [ "$DRY" = "1" ]; then
    log "DRY would reap $entry ($sz)"
  else
    rm -rf -- "$entry" && { log "REAPED $entry ($sz)"; reaped=$((reaped + 1)); } \
                        || log "ERROR could not reap $entry"
  fi
done <<< "$candidates"

log "DONE dry=$DRY reaped=$reaped remaining=$(du -sh "$STAGE_ROOT" 2>/dev/null | cut -f1) free=$(df -h "$(dirname "$STAGE_ROOT")" 2>/dev/null | awk 'NR==2{print $4" ("$5")"}')"
exit 0
