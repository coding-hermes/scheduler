package scheduler

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func schedgap17007Board(t *testing.T, rows ...string) string {
	t.Helper()
	wd := t.TempDir()
	dir := filepath.Join(wd, ".coding-hermes", "board")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tasks.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, row := range rows {
		var obj map[string]any
		if err := json.Unmarshal([]byte(row), &obj); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(row + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	return wd
}

func TestSCHEDGAP17007TasksAdmissionPreflight(t *testing.T) {
	cases := []struct {
		name       string
		rows       []string
		actionable bool
	}{
		{"actionable", []string{`{"id":"A","status":"pending","acceptance_criteria":["run tests"]}`}, true},
		{"blocked", []string{`{"id":"A","status":"pending","depends_on":["B"],"acceptance_criteria":["run tests"]}`, `{"id":"B","status":"pending"}`}, false},
		{"no-criteria", []string{`{"id":"A","status":"pending"}`}, false},
		{"owner-gated", []string{`{"id":"A","status":"blocked","blocked_reason":"owner"}`}, false},
		{"in-flight", []string{`{"id":"A","status":"pending","worker_status":"claimed","acceptance_criteria":["run tests"]}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wd := schedgap17007Board(t, tc.rows...)
			got, ok := tasksAdmissionPreflight(wd)
			if !ok {
				t.Fatal("preflight did not read fixture")
			}
			if (got.Actionable > 0) != tc.actionable {
				t.Fatalf("counts=%+v actionable=%v want %v", got, got.Actionable > 0, tc.actionable)
			}
		})
	}
}

func TestSCHEDGAP17007CheapSkipCounterAndLog(t *testing.T) {
	wd := admitWorkdirWithBoard(t, `{"id":"A","status":"pending"}`)
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) })
	before := CheapAdmissionSkipCount()
	if !tasksBuilderAdmissionBlocked("gap17007", wd, "tasks", "owner", "") {
		t.Fatal("expected no-actionable board to be blocked")
	}
	if CheapAdmissionSkipCount() != before+1 {
		t.Fatalf("cheap skip count = %d, want %d", CheapAdmissionSkipCount(), before+1)
	}
	if !strings.Contains(buf.String(), "ADMISSION-CHEAP-SKIP: project=gap17007 reason=tasks_no_actionable") {
		t.Fatalf("missing stable cheap-skip line: %s", buf.String())
	}
}
