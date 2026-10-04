package scheduler

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1680 — non-commit side effects are tick artifacts.
//
// `commits=0` is the fleet's de-facto success test today, and it can only see
// GIT artifacts: a lane whose deliverable is not a commit is scored as a
// wasted tick. The sync family is the proof — every sync lane showed the same
// 7-ticks / 6-zero-commit shape (85% "no-op") while auger-sync's own tick
// reported "Keys written (5 total, all verified)" with commits=0. So the
// no-op determination (lifecycle.terminalOutcome) consults a NON-COMMIT
// side-effect count alongside the git artifacts: the DuckBrain memory-key
// writes this tick's session actually performed.
//
// MEASUREMENT SURFACE: the tick's gateway session transcript in the Hermes
// state store — the same read-only, 2s-bounded seam the builder guard already
// uses (builder_guard.go observeSessionTelemetry). The session_key IS the
// tick id for gateway spawns, so one indexed lookup plus a bounded transcript
// scan yields the count. Exec/local spawns carry no such mapping and read 0,
// and the git-artifact path still governs them. Any failure (no state store,
// no session row, unreadable transcript) reads 0: this signal can only ADD
// productivity, never manufacture a no-op — the SCHED-GAP-1652 doctrine that
// a measurement gap must never fabricate evidence of idleness.

// memoryWriteVerbs are the write-side verbs a memory-store tool name can
// carry. A tool name must ALSO name the memory store (see
// memoryWriteToolName) before any of these matters, so a generic
// write_file / str_replace call can never be misread as a memory write.
var memoryWriteVerbs = []string{"remember", "write", "append", "create", "put", "post"}

// memoryWriteToolName reports whether a session tool-call NAME is a DuckBrain
// / memory-store WRITE — the non-commit artifact this row counts. The rule is
// deliberately conservative in both directions:
//
//   - the name must reference the memory store ("duckbrain" or "memory"), so
//     the fleet's file/board tools are never counted;
//   - the name must carry a write verb, so a read-side call (recall, search,
//     list, get) is never counted as a write.
//
// An unrecognized naming convention therefore reads as NOT a write — the
// count can under-report, which only falls back to the git artifacts; it can
// never over-report productivity.
func memoryWriteToolName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	if !strings.Contains(n, "duckbrain") && !strings.Contains(n, "memory") {
		return false
	}
	for _, v := range memoryWriteVerbs {
		if strings.Contains(n, v) {
			return true
		}
	}
	return false
}

// countMemoryKeysInSession counts the DuckBrain memory-key writes the tick's
// gateway session observed (SCHED-GAP-1680). 0 on every unmeasurable path —
// see the file header for why that direction is the safe one.
func countMemoryKeysInSession(tickID string) int {
	if strings.TrimSpace(tickID) == "" {
		return 0
	}
	path := hermesStateDBPath()
	if path == "" {
		return 0
	}
	if _, err := os.Stat(path); err != nil {
		return 0 // no agent state store on this host: nothing observed
	}
	sdb, err := database.OpenHermesStateReadOnly(path)
	if err != nil {
		return 0
	}
	defer sdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), hermesGuardQueryTimeout)
	defer cancel()

	// Assistant rows carry the tool_calls JSON. Tool RESULT rows carry an
	// empty tool_calls column, so this scan counts one entry per memory-write
	// CALL — no call/result double counting. Bounded like the guard's scan.
	rows, err := sdb.QueryContext(ctx, `
SELECT COALESCE(tool_calls, '')
FROM messages
WHERE session_id = (SELECT id FROM sessions WHERE session_key = ? ORDER BY started_at DESC LIMIT 1)
ORDER BY timestamp DESC LIMIT 200`, tickID)
	if err != nil {
		return 0
	}
	defer rows.Close()

	keys := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			continue
		}
		for _, name := range toolCallsFromAssistantRow(raw) {
			if memoryWriteToolName(name) {
				keys++
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("MEMORY-KEYS: %s transcript scan failed: %v (reading 0)", tickID, err)
		return 0
	}
	if keys > 0 {
		log.Printf("MEMORY-KEYS: %s observed %d DuckBrain memory write(s) — non-commit artifact (SCHED-GAP-1680)", tickID, keys)
	}
	return keys
}
