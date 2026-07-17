#!/usr/bin/env bash
# scheduler-verify — runs the built-in scheduler correctness test
# Intended to run every 2 hours via host crontab.
# Exit 0 = all checks pass, exit 1 = verification failed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$SCRIPT_DIR"

BIN="${SCRIPT_DIR}/bin/schedulerd"
LOG="${SCRIPT_DIR}/deploy/verify-$(date +%Y%m%d-%H%M%S).log"

if [ ! -x "$BIN" ]; then
    echo "❌ SCHEDULER VERIFY: binary not found at $BIN" | tee "$LOG"
    exit 1
fi

echo "=== SCHEDULER VERIFY $(date -u -Iseconds) ===" | tee "$LOG"
"$BIN" --test-verify 3 2>&1 | tee -a "$LOG"
EXIT_CODE=$?

if [ $EXIT_CODE -eq 0 ]; then
    echo "✅ SCHEDULER VERIFIED" | tee -a "$LOG"
else
    echo "❌ SCHEDULER VERIFY FAILED (exit $EXIT_CODE)" | tee -a "$LOG"
fi

# Cleanup logs older than 30 days
find "${SCRIPT_DIR}/deploy" -name 'verify-*.log' -mtime +30 -delete 2>/dev/null || true

exit $EXIT_CODE
