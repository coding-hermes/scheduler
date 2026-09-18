package scheduler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Board status vocabulary gate (SCHED-GAP-164).
//
// The board is written by many hands (foremen, workers, the PM stand-in, ad-hoc
// scripts) but only ONE spelling is dispatchable: the foreman prompt picks
// status=="pending" rows and the pending-boost counter (board_awareness.go,
// CountPending) counts only those. The scheduler's READERS deliberately accept a
// wider open vocabulary (openStatuses in board_freshness.go, boardOpenRows in
// adaptive_cooldown.go) for forward compatibility, so a row minted with one of
// those wider spellings ("todo", "open", "in_progress", ...) is visible to the
// fleet and scheduled by nobody — visible work that no lane will ever pick up.
//
// This file is the writer-side half of that contract: it names the vocabulary
// writers must use and reports, read-only, every row outside it. The daemon's
// readers are NOT narrowed here (forward compatibility stays intact); this is a
// gate on new writes, not a change to what the scheduler can see.
//
// BoardAllowedStatuses is the closed WRITER vocabulary. It is mirrored by the
// Python regression gate (ops/check-fleet-invariants.py, BOARD_ALLOWED_STATUSES,
// check 8 "board-vocab"); the two MUST stay in lockstep, which
// TestBoardVocabValidator_AllowedSetMatchesPythonGate pins.
var BoardAllowedStatuses = []string{"pending", "complete", "duplicate"}

// BoardLegacyClosedStatuses are the closed-state spellings the board carried
// before the vocabulary was closed (4 rows still carried them on 2026-09-18).
// They are off-vocabulary too, but they mark FINISHED work rather than parked
// work, so the Python gate reports them as a separate class
// ("board-legacy-status") that the PM cycle can sweep to "complete" in the same
// tick instead of treating them as a writer bug. ValidateBoardVocab reports them
// with the same off-vocabulary shape — the split is the caller's policy.
var BoardLegacyClosedStatuses = []string{"done", "completed", "closed"}

// ValidateBoardVocab reads a JSONL board file and returns one entry per row
// whose status is outside BoardAllowedStatuses, formatted "<id>: status=<value>".
//
// Read-only: the board is never rewritten and no status is normalised here — the
// caller decides what to do with a finding.
//
// Tolerance, deliberately: lines that fail to parse as a JSON object are skipped,
// because malformed JSONL is the concern of the board scanner, not of this
// vocabulary gate. A missing (or empty-path) board file is NOT an error and
// returns (nil, nil), so callers can run the gate in environments without a
// board (test rigs, old-style workdirs, a checkout without the board dir).
//
// Status normalisation is str().lower().strip(): an absent or JSON-null status
// reads as "" (which the daemon's openStatuses counts as open but the foreman
// prompt never picks — hence still a finding), and a non-string status is
// stringified rather than silently accepted.
func ValidateBoardVocab(boardPath string) ([]string, error) {
	if strings.TrimSpace(boardPath) == "" {
		return nil, nil
	}
	f, err := os.Open(boardPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no board → nothing to validate, not an error
		}
		return nil, err
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue // malformed line — a different gate's concern
		}
		if row == nil {
			continue // decodes to nil for "null" / a non-object
		}
		if boardStatusAllowed(boardRowStatus(row)) {
			continue
		}
		out = append(out, boardRowID(row)+": status="+boardRowStatus(row))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// boardStatusAllowed reports whether status is in the closed writer vocabulary.
func boardStatusAllowed(status string) bool {
	for _, s := range BoardAllowedStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// boardRowStatus normalises a row's status: absent/null → "", otherwise the
// lowercased, trimmed string form. Mirrors the Python gate's
// "" if raw is None else str(raw).lower().strip().
func boardRowStatus(row map[string]json.RawMessage) string {
	raw, ok := row["status"]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.ToLower(strings.TrimSpace(s))
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil || v == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
}

// boardRowID resolves a row's identifier for reporting: "id", then "task_id",
// then "<unknown>" (mirrors the Python gate's board_row_id).
func boardRowID(row map[string]json.RawMessage) string {
	for _, key := range []string{"id", "task_id"} {
		raw, ok := row[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if id := strings.TrimSpace(s); id != "" {
				return id
			}
		}
	}
	return "<unknown>"
}
