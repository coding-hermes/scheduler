package scheduler

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// ClosureViolation describes a closed board row that carries no closure
// evidence. A row is CLOSED only when status=="complete" AND
// worker_status=="complete" AND completed_at is non-empty; a violation is a
// closed row where ALL of reasoning/commit_hash/worker_summary are empty.
type ClosureViolation struct {
	ID            string   `json:"id"`
	MissingFields []string `json:"missing_fields"`
	CompletedAt   string   `json:"completed_at"`
}

// boardEvidenceFields are the fields whose collective absence makes a closed
// row unverifiable: a user auditing the board cannot tell what shipped.
var boardEvidenceFields = []string{"reasoning", "commit_hash", "worker_summary"}

// BoardClosureViolations scans a board JSONL file and returns the closed rows
// that carry NO closure evidence. Rows that are not closed — pending rows, or
// status=complete with worker_status!=complete or completed_at null
// (legacy/perpetual fixtures by design, see board_awareness.go GAP-036) — are
// ignored. Malformed lines are skipped, never fatal. Non-JSONL paths (e.g. the
// .coding-hermes/tasks.md fallback) have no evidence fields to check and
// return no violations.
func BoardClosureViolations(path string) ([]ClosureViolation, error) {
	if !strings.HasSuffix(path, ".jsonl") {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var violations []ClosureViolation
	scanner := bufio.NewScanner(f)
	// Allow lines up to 1MB — board entries can carry large detail blobs
	// (same budget as countPendingBoard in board_awareness.go).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue // malformed line — skip, never crash
		}
		if !boardRowClosed(obj) {
			continue
		}
		var missing []string
		for _, field := range boardEvidenceFields {
			if boardFieldEmpty(obj[field]) {
				missing = append(missing, field)
			}
		}
		if len(missing) == len(boardEvidenceFields) {
			violations = append(violations, ClosureViolation{
				ID:            boardString(obj["id"]),
				MissingFields: missing,
				CompletedAt:   boardString(obj["completed_at"]),
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return violations, nil
}

// boardRowClosed reports whether a row is genuinely CLOSED: status=="complete"
// AND worker_status=="complete" AND completed_at non-empty. Rows with
// status=complete but worker_status=pending (or completed_at null) are
// legacy/perpetual fixtures, NOT closures — a closure-evidence gate must
// ignore them (board_awareness.go GAP-036 comment block).
func boardRowClosed(obj map[string]json.RawMessage) bool {
	if boardString(obj["status"]) != "complete" {
		return false
	}
	if boardString(obj["worker_status"]) != "complete" {
		return false
	}
	return strings.TrimSpace(boardString(obj["completed_at"])) != ""
}

// boardFieldEmpty reports whether a raw JSON field value is missing or
// whitespace-only. JSON null, absent keys, and "" all count as empty.
func boardFieldEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Non-string values (numbers, objects) are not usable as evidence
		// text — treat as empty so they cannot satisfy the evidence rule.
		return true
	}
	return strings.TrimSpace(s) == ""
}

// boardString extracts a string field, tolerating JSON null / absent keys.
func boardString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
