package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEventsFile writes a JSONL events file into a temp dir and returns its
// path. Raw lines are used verbatim so the test can mint the exact event shapes
// the live log carries (including malformed lines and id-less annotation rows).
func writeEventsFile(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestValidateEventIDs_CleanAscendingIsSilent is the row's happy criterion: a
// log of ascending 19-digit ids (the shape every writer must mint) yields no
// finding at all, including one with gaps — boardctl tolerates gaps, so
// expecting a dense sequence here would paint every live board red.
func TestValidateEventIDs_CleanAscendingIsSilent(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1789977500000000001, "event_type": "task_created", "task_id": "SCHED-GAP-206"}`,
		`{"id": 1789977500000000002, "event_type": "task_updated", "task_id": "SCHED-GAP-206"}`,
		`{"id": 1790000000000000001, "event_type": "audit", "task_id": null}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ascending 19-digit ids must produce no findings, got %d: %v", len(got), got)
	}
}

// TestValidateEventIDs_SubScaleIDIsCaught is the reported defect's own shape,
// reduced: a line minted at epoch-MICROSECOND scale (16 digits, below 10^18)
// appended after 19-digit ids. It must be named with its line number and id.
func TestValidateEventIDs_SubScaleIDIsCaught(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1789977500000000030, "event_type": "task_created"}`,
		`{"id": 1789977500000000031, "event_type": "task_created"}`,
		`{"id": 1790080373408208, "event_type": "audit"}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	want := []string{
		"3: id=1790080373408208 is below the 19-digit floor 1000000000000000000 " +
			"(event ids must stay on the board's 19-digit scale)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("finding[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if !strings.Contains(got[0], "3:") || !strings.Contains(got[0], "1790080373408208") {
		t.Fatalf("finding must name the line number and the bad id: %q", got[0])
	}
}

// TestValidateEventIDs_DescendingIDIsCaught covers the order half of the rule: a
// 19-digit id that sorts BELOW the highest id already in the file (the exact
// shape boardctl reported as "ids must ascend") is a finding; ascending ids
// after it are not.
func TestValidateEventIDs_DescendingIDIsCaught(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1789977500000000011, "event_type": "audit"}`,
		`{"id": 1789977500000000005, "event_type": "audit"}`,
		`{"id": 1789977500000000012, "event_type": "audit"}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	want := "2: id=1789977500000000005 descends below earlier id 1789977500000000011 " +
		"(ids must ascend; gaps tolerated)"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want exactly [%q]", got, want)
	}
}

// TestValidateEventIDs_OffScaleFileIsEntirelyReported pins the no-anchor rule:
// a log whose ids are ALL below 10^18 establishes no scale at all, so every
// int id in it is off-scale — that is the shape a wrong-basis writer produces
// into a new log, and reporting it is the point of the gate.
func TestValidateEventIDs_OffScaleFileIsEntirelyReported(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1790080373408208}`,
		`{"id": 1790080400000000}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 findings (every int id off-scale), got %d: %v", len(got), got)
	}
	for i, wantPrefix := range []string{"1: id=1790080373408208 ", "2: id=1790080400000000 "} {
		if !strings.HasPrefix(got[i], wantPrefix) {
			t.Fatalf("finding[%d] = %q, want prefix %q", i, got[i], wantPrefix)
		}
	}
	if !strings.Contains(got[0], "the file carries no id at that scale") {
		t.Fatalf("no-anchor finding must say so: %q", got[0])
	}
}

// TestValidateEventIDs_LiveBoardLegacyPrefixIsTolerated pins the tolerance that
// keeps the gate usable on a real board: this project's events.jsonl carries
// 979 pre-scale ids (lines 1-999: 1,2,3…, then a 10-digit and a 13-digit era)
// BEFORE its first 19-digit id. The board is append-only and those lines cannot
// be renumbered, so they must not be reported — while the scale region after
// the anchor still asserts. Without this pin, one strict-by-the-book refactor
// turns the CI battery red for history nobody can change.
func TestValidateEventIDs_LiveBoardLegacyPrefixIsTolerated(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1, "event_type": "legacy"}`,
		`{"id": 2, "event_type": "legacy"}`,
		`{"id": 1789703183, "event_type": "legacy-epoch-seconds"}`,
		`{"id": 1789872174749, "event_type": "legacy-epoch-millis"}`,
		`{"id": 1789915599706355200, "event_type": "task_completed"}`, // scale anchor
		`{"id": 1789915599706355201, "event_type": "task_updated"}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("pre-scale legacy prefix must be tolerated, got %d findings: %v", len(got), got)
	}
}

// TestValidateEventIDs_LegacyEventShapesAreSkipped pins the skip arms: no id
// field, null id, string ids (the live board carries "PM-001",
// "evt-1789703180", "EVT-PM-20260920-001", ""), a bool id, and malformed JSON
// produce no finding — those lines are legacy shapes, not scale violations.
func TestValidateEventIDs_LegacyEventShapesAreSkipped(t *testing.T) {
	path := writeEventsFile(t,
		`{"kind": "refile_suppressed", "detail": "no id at all"}`,
		`{"id": null, "event_type": "audit"}`,
		`{"id": "", "event_type": "audit"}`,
		`{"id": "PM-001", "event_type": "task_added"}`,
		`{"id": "EVT-PM-20260920-001", "event_type": "audit"}`,
		`{"id": true, "event_type": "audit"}`,
		`not json at all`,
		`["not", "an", "object"]`,
		``,
		`{"id": 1789977500000000001, "event_type": "task_created"}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("legacy event shapes must be skipped, got %d findings: %v", len(got), got)
	}
}

// TestValidateEventIDs_DuplicateIDIsTolerated pins the boardctl parity on
// re-used ids: the live log carries two duplicate 19-digit ids (lines 1069 and
// 1128, both re-writes that re-used an id already in the file) which boardctl
// warns about rather than failing. The repo gate must not gate them either, or
// every live board is permanently red.
func TestValidateEventIDs_DuplicateIDIsTolerated(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1789977500000000011, "event_type": "audit", "timestamp": "2026-09-21T10:42:59"}`,
		`{"id": 1789977500000000011, "event_type": "audit", "timestamp": "2026-09-21T11:12:58"}`,
		`{"id": 1789977500000000068, "event_type": "task_updated", "timestamp": "2026-09-23T00:52:34"}`,
		`{"id": 1789977500000000069, "event_type": "task_updated", "timestamp": "2026-09-23T00:53:56"}`,
		`{"id": 1789977500000000068, "event_type": "wave_recovery", "timestamp": "2026-09-23T00:55:00"}`,
		`{"id": 1789977500000000070, "event_type": "task_created", "timestamp": "2026-09-23T02:28:44"}`,
	)

	got, err := ValidateEventIDs(path)
	if err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("duplicate ids are tolerated (boardctl warns): got %d findings %v", len(got), got)
	}
}

// TestValidateEventIDs_EmptyAndMissingFilesAreNotErrors pins the skip contract:
// an empty file, a missing file and an empty path all return (nil, nil) so the
// gate runs in a checkout or test rig without a board.
func TestValidateEventIDs_EmptyAndMissingFilesAreNotErrors(t *testing.T) {
	empty := writeEventsFile(t)
	for _, path := range []string{
		empty,
		filepath.Join(t.TempDir(), "nope", "events.jsonl"),
		"",
	} {
		got, err := ValidateEventIDs(path)
		if err != nil || got != nil {
			t.Fatalf("ValidateEventIDs(%q) = (%v, %v), want (nil, nil)", path, got, err)
		}
	}
}

// TestValidateEventIDs_IsReadOnly pins that the validator reports without
// rewriting the log — the corpus must be byte-identical afterwards.
func TestValidateEventIDs_IsReadOnly(t *testing.T) {
	path := writeEventsFile(t,
		`{"id": 1789977500000000031, "event_type": "task_created"}`,
		`{"id": 1790080373408208, "event_type": "audit"}`,
	)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := ValidateEventIDs(path); err != nil {
		t.Fatalf("ValidateEventIDs: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("events file was rewritten:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestValidateEventIDs_ScaleMatchesPythonGate pins the Go/Python parity the row
// requires: ops/check-fleet-invariants.py's EVENT_ID_SCALE and its class name
// must name the same floor and the same class as EventIDScale here. Without
// this, the CI gate and the in-process validator can drift apart and both read
// green while enforcing different scales.
func TestValidateEventIDs_ScaleMatchesPythonGate(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "ops", "check-fleet-invariants.py"))
	if err != nil {
		t.Fatalf("read python gate: %v", err)
	}
	text := string(src)

	if want := "EVENT_ID_SCALE = 10 ** 18"; !strings.Contains(text, want) {
		t.Fatalf("python gate must declare %q (found no such constant)", want)
	}
	if want := fmt.Sprintf("EventIDScale int64 = %d", EventIDScale); !strings.Contains(text, want) {
		t.Fatalf("python gate must document the Go constant %q", want)
	}
	if want := `EVENT_ID_CLASS = "event-id-ascending"`; !strings.Contains(text, want) {
		t.Fatalf("python gate must declare %q (the class name this validator mirrors)", want)
	}
}
