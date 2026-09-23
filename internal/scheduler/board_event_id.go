package scheduler

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Event id scale + ascent gate (SCHED-GAP-206).
//
// `events.jsonl` is the board's append-only log: every writer (the foreman
// tick, the PM stand-in, boardctl's `event`/`update`/`create`, the supervisor)
// mints an integer id for the line it appends. The board has ONE id scale — the
// 19-digit epoch-NANOSECOND scale, ids >= 10^18 — and within the file the ids
// must never descend.
//
// Why this exists: on 2026-09-22 two lines were appended at epoch-MICROSECOND
// scale (16 digits, `1790080373408208`) while the file's existing ids sat at
// `1789977500000000031`. The root cause was a writer that picked its id basis
// from an epoch/constant source instead of reading the file's existing max
// id, and the only thing that noticed was the live board gate — `boardctl
// validate`, which FAILed with "event id … descends below earlier id … (ids
// must ascend)". The PM repaired the two lines by hand; the WRITER was still
// free to re-descend the file on its next write. This validator is the
// repo-side half of that fix: it names the offending LINE and the offending ID,
// so the next off-scale write fails a test instead of a live board gate.
//
// Relationship to boardctl: this mirrors boardctl's own `validateEvents`
// (github.com/coding-hermes/boardctl, internal/board/validate.go) — the same
// "ids must ascend; gaps tolerated" rule and the same tolerance for legacy
// event shapes — and adds the one assertion boardctl deliberately does not
// make, that an id must sit at the board's 19-digit scale at all. The upside
// of the scale assertion is that a wrong-scale id is caught even when it
// happens to sort ABOVE the file's current max (boardctl's order-only rule is
// blind to that).
//
// Tolerances, deliberately:
//
//   - a line that fails to parse as JSON is skipped: malformed JSONL is the
//     board scanner's concern, not this gate's (same rule as
//     ValidateBoardVocab);
//   - a line with no "id" field, or with a non-int id, is skipped: the live log
//     legitimately carries string ids ("PM-001", "evt-1789703180",
//     "EVT-PM-20260920-001"), empty-string ids and id-less annotation rows;
//   - a DUPLICATE id (one already seen in the file) is skipped, not reported:
//     re-used ids are benign legacy rows on live boards and boardctl warns
//     rather than errors on them, so gating them here would paint every board
//     red for history no writer can change;
//   - the file's PRE-SCALE PREFIX is skipped: ids that appear BEFORE the file's
//     first >= 10^18 id are pre-scale history (this board carries 979 of them,
//     lines 1-999). The board is append-only and those lines cannot be
//     renumbered, so the gate enforces the scale from the file's own scale
//     anchor onward. A file with NO id at the 19-digit scale is a different
//     thing entirely — nothing establishes a scale, so every int id in it is
//     off-scale and every one is reported;
//   - a missing (or empty-path) file is NOT an error and returns (nil, nil), so
//     the gate runs in environments without a board (test rigs, a checkout
//     without the board dir).
//
// Mirrored by the Python gate's check 11 (`ops/check-fleet-invariants.py`,
// class "event-id-ascending"); the floor constant is pinned equal by
// TestEventIDScaleMatchesPythonGate, and the finding text is shared verbatim
// with the Python detail strings.
const EventIDScale int64 = 1_000_000_000_000_000_000

// ValidateEventIDs reads a JSONL events file and returns one finding per line
// whose integer event id is off-scale (below EventIDScale) or descends below
// the highest id already seen in the file. Findings read "<line>: <detail>",
// in file order, e.g.
//
//	1090: id=1790080373408208 is below the 19-digit floor 1000000000000000000 (event ids must stay on the board's 19-digit scale)
//
// Read-only: the file is never rewritten and no id is normalised here — the
// caller decides what to do with a finding. A clean file returns (nil, nil), as
// does a file that does not exist.
func ValidateEventIDs(eventsPath string) ([]string, error) {
	if strings.TrimSpace(eventsPath) == "" {
		return nil, nil
	}
	f, err := os.Open(eventsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no events file → nothing to validate, not an error
		}
		return nil, err
	}
	defer f.Close()

	// Pass 1 collects the (line, id) pairs; the scale anchor (does the file
	// carry ANY 19-digit id?) decides whether the leading sub-scale ids are
	// pre-scale history or a wholly off-scale file.
	type eventID struct {
		line int
		id   int64
	}
	var ids []eventID
	hasScale := false
	lineNo := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &row); err != nil || row == nil {
			continue // malformed line — a different gate's concern
		}
		id, ok := eventIntID(row)
		if !ok {
			continue // no id, or a non-int id (legacy shape tolerated)
		}
		ids = append(ids, eventID{line: lineNo, id: id})
		if id >= EventIDScale {
			hasScale = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var out []string
	seen := make(map[int64]bool, len(ids))
	var max int64
	haveMax := false
	scaled := false // the file's scale anchor has been reached
	for _, e := range ids {
		if !hasScale {
			// No 19-digit id anywhere: nothing establishes the board's scale,
			// so every int id in the file is off-scale (the shape a writer on
			// the wrong basis produces into an empty/new log).
			out = append(out, fmt.Sprintf("%d: %s", e.line, eventIDNoScaleDetail(e.id)))
			continue
		}
		if !scaled && e.id < EventIDScale {
			continue // pre-scale history — append-only, cannot be renumbered
		}
		scaled = true
		if seen[e.id] {
			continue // duplicate id — boardctl warns, this gate does not gate it
		}
		seen[e.id] = true
		if e.id < EventIDScale {
			out = append(out, fmt.Sprintf("%d: %s", e.line, eventIDBelowFloorDetail(e.id)))
			continue
		}
		if haveMax && e.id < max {
			out = append(out, fmt.Sprintf("%d: %s", e.line, eventIDDescentDetail(e.id, max)))
			continue
		}
		if !haveMax || e.id > max {
			max = e.id
			haveMax = true
		}
	}
	return out, nil
}

// eventIntID extracts a row's numeric event id. Only a JSON integer counts:
// absent, null, string, bool and float shapes are all "not an event id" and are
// skipped by the validator, mirroring the Python gate's `isinstance(value, int)
// and not isinstance(value, bool)` rule.
func eventIntID(row map[string]json.RawMessage) (int64, bool) {
	raw, ok := row["id"]
	if !ok {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

// eventIDBelowFloorDetail is the finding text for an id below EventIDScale in a
// file that does carry 19-digit ids. Shared verbatim with the Python gate.
func eventIDBelowFloorDetail(id int64) string {
	return fmt.Sprintf("id=%d is below the 19-digit floor %d (event ids must stay on the board's 19-digit scale)",
		id, EventIDScale)
}

// eventIDNoScaleDetail is the finding text for an id in a file with NO id at
// the 19-digit scale — nothing establishes a scale, so every int id is
// off-scale. Shared verbatim with the Python gate.
func eventIDNoScaleDetail(id int64) string {
	return fmt.Sprintf("id=%d is below the 19-digit floor %d and the file carries no id at that scale (every int id in it is off-scale)",
		id, EventIDScale)
}

// eventIDDescentDetail is the finding text for an id that descends below the
// highest id already in the file. Wording mirrors boardctl's validateEvents
// error. Shared verbatim with the Python gate.
func eventIDDescentDetail(id, max int64) string {
	return fmt.Sprintf("id=%d descends below earlier id %d (ids must ascend; gaps tolerated)", id, max)
}
