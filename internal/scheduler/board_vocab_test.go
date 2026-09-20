package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeVocabBoard writes a JSONL board file into a temp dir and returns its path.
// Raw lines are used verbatim so the test can mint the exact row shapes the
// live board carries (including malformed lines).
func writeVocabBoard(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.jsonl")
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestBoardVocabValidator_FlagsOffVocabulary is the row's named criterion: a
// board with one off-vocabulary row ("todo") and one dispatchable row
// ("pending") yields exactly one violation, naming the todo row, and leaves the
// pending row alone.
func TestBoardVocabValidator_FlagsOffVocabulary(t *testing.T) {
	path := writeVocabBoard(t,
		`{"id": "SCHED-GAP-900", "status": "todo", "title": "parked by an old writer"}`,
		`{"id": "SCHED-GAP-901", "status": "pending", "title": "dispatchable"}`,
	)

	got, err := ValidateBoardVocab(path)
	if err != nil {
		t.Fatalf("ValidateBoardVocab: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 violation, got %d: %v", len(got), got)
	}
	if want := "SCHED-GAP-900: status=todo"; got[0] != want {
		t.Fatalf("violation = %q, want %q", got[0], want)
	}
	for _, v := range got {
		if strings.Contains(v, "SCHED-GAP-901") {
			t.Fatalf("pending row must not be flagged: %q", v)
		}
	}
}

// TestBoardVocabValidator_InProgressIsAllowed is the positive pin for
// RELEASE-007: "in_progress" is the live-lane claim the foreman wave writes at
// dispatch, so a board of ONLY in_progress rows must produce NO finding. If
// this fails, the wave's dispatch marker is being reported as a writer bug
// again (the 2026-09-20 CI red on 9e5b712).
func TestBoardVocabValidator_InProgressIsAllowed(t *testing.T) {
	path := writeVocabBoard(t,
		`{"id": "TRBL-026", "status": "in_progress", "title": "live lane holds this"}`,
		`{"id": "RELEASE-007", "status": "in_progress"}`,
	)

	got, err := ValidateBoardVocab(path)
	if err != nil {
		t.Fatalf("ValidateBoardVocab: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("in_progress rows must not be flagged, got %d findings: %v", len(got), got)
	}
}

// TestBoardVocabValidator_VocabularyCoverage pins the rest of the status matrix:
// every allowed spelling passes, every other spelling is reported once, the
// legacy closed spellings are reported with the same shape (the Python gate
// splits them into its own class), normalisation is case/space tolerant, and
// malformed / non-object / absent-null-status lines behave as documented.
func TestBoardVocabValidator_VocabularyCoverage(t *testing.T) {
	path := writeVocabBoard(t,
		`{"id": "OK-1", "status": "pending"}`,
		`{"id": "OK-2", "status": "complete"}`,
		`{"id": "OK-3", "status": "duplicate"}`,
		`{"id": "OK-4", "status": "  PENDING "}`,
		`{"id": "OK-5", "status": "in_progress"}`,
		`{"id": "OK-6", "status": " IN_PROGRESS "}`,
		`{"id": "LEG-1", "status": "done"}`,
		`{"id": "LEG-2", "status": "completed"}`,
		`{"id": "LEG-3", "status": "closed"}`,
		`{"id": "OFF-1", "status": "todo"}`,
		`{"id": "OFF-2", "status": "rework"}`,
		`{"id": "OFF-3", "status": "TODo"}`,
		`{"id": "OFF-4"}`,
		`{"id": "OFF-5", "status": null}`,
		`{"task_id": "OFF-6", "status": "open"}`,
		`not json at all`,
		`["not", "an", "object"]`,
		``,
	)

	got, err := ValidateBoardVocab(path)
	if err != nil {
		t.Fatalf("ValidateBoardVocab: %v", err)
	}
	want := []string{
		"LEG-1: status=done",
		"LEG-2: status=completed",
		"LEG-3: status=closed",
		"OFF-1: status=todo",
		"OFF-2: status=rework",
		"OFF-3: status=todo",
		"OFF-4: status=",
		"OFF-5: status=",
		"OFF-6: status=open",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("finding[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestBoardVocabValidator_MissingBoardIsNotAnError pins the skip contract: a
// missing file (and an empty path) returns (nil, nil) so the gate can run in
// environments without a board.
func TestBoardVocabValidator_MissingBoardIsNotAnError(t *testing.T) {
	for _, path := range []string{
		filepath.Join(t.TempDir(), "nope", "tasks.jsonl"),
		"",
	} {
		got, err := ValidateBoardVocab(path)
		if err != nil || got != nil {
			t.Fatalf("ValidateBoardVocab(%q) = (%v, %v), want (nil, nil)", path, got, err)
		}
	}
}

// TestBoardVocabValidator_IsReadOnly pins that the helper reports without
// rewriting the board — the corpus must be byte-identical afterwards.
func TestBoardVocabValidator_IsReadOnly(t *testing.T) {
	path := writeVocabBoard(t, `{"id": "OFF-1", "status": "todo"}`, `{"id": "OK-1", "status": "pending"}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := ValidateBoardVocab(path); err != nil {
		t.Fatalf("ValidateBoardVocab: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("board was rewritten:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestBoardVocabValidator_AllowedSetMatchesPythonGate pins the Go/Python parity
// the row requires: ops/check-fleet-invariants.py's BOARD_ALLOWED_STATUSES tuple
// and BOARD_LEGACY_CLOSED_STATUSES tuple must name exactly the same statuses as
// BoardAllowedStatuses / BoardLegacyClosedStatuses here. Without this, the CI
// gate and the in-process rule can drift apart and both read green.
func TestBoardVocabValidator_AllowedSetMatchesPythonGate(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "ops", "check-fleet-invariants.py"))
	if err != nil {
		t.Fatalf("read python gate: %v", err)
	}
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"BOARD_ALLOWED_STATUSES", BoardAllowedStatuses},
		{"BOARD_LEGACY_CLOSED_STATUSES", BoardLegacyClosedStatuses},
	} {
		got := pythonTuple(t, string(src), tc.name)
		if len(got) != len(tc.want) {
			t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s = %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}

// pythonTuple extracts a module-level `NAME = ("a", "b")` tuple literal from the
// Python gate's source. Deliberately dumb: the gate's constants are single-line
// literals by design.
func pythonTuple(t *testing.T, src, name string) []string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, name+" = (") {
			continue
		}
		body := strings.TrimSuffix(strings.TrimPrefix(line, name+" = ("), ")")
		var out []string
		for _, part := range strings.Split(body, ",") {
			part = strings.Trim(strings.TrimSpace(part), `"'`)
			if part != "" {
				out = append(out, part)
			}
		}
		return out
	}
	t.Fatalf("%s tuple not found in ops/check-fleet-invariants.py", name)
	return nil
}
