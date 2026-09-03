package blocks

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseBoardRowJSON parses one tasks.jsonl line back into a map for shape
// assertions.
func parseBoardRowJSON(t *testing.T, line string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("board row not valid JSON: %v\nline: %s", err, line)
	}
	return m
}

// makeBoard creates a workdir with an (empty or seeded) JSONL task board and
// returns the workdir + board path. seed "" still creates the (empty) file —
// a workdir WITHOUT the file is the ErrNoBoard case, tested separately.
func makeBoard(t *testing.T, seed string) (string, string) {
	t.Helper()
	wd := t.TempDir()
	path := filepath.Join(wd, ".coding-hermes", "board", "tasks.jsonl")
	writeBoardFile(t, path, seed)
	return wd, path
}

func readBoardLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// deployRows builds rows for template/project/date (mirrors Deploy's use).
func deployRows(tpl Template, project, date, group string) []BoardTaskRow {
	note := fmt.Sprintf("deployed via scheduler template %s to group %s on %s", tpl.Name, group, date)
	return BuildTaskRows(tpl, project, date, note)
}

func sampleTemplate() Template {
	return Template{
		Name: "QUALITY-SWEEP",
		Tasks: []TemplateTask{
			{Title: "Audit {PROJECT} board hygiene", Detail: "Full board audit on {PROJECT}", Labels: []string{"audit"}},
			{Title: "Close stale rows on {PROJECT}", Labels: []string{"hygiene"}},
		},
	}
}

// boardTestCase is one row of the appender table.
type boardTestCase struct {
	name        string
	seed        string // existing board content ("" = empty board file)
	rows        []BoardTaskRow
	wantAppend  int   // number of ids appended (0 = skipped or error)
	wantSkip    bool  // idempotent skip expected
	wantErrKind error // ErrWorkdirMissing / ErrNoBoard, or nil
	wantLines   int   // expected final board line count
	checkClosed bool  // whether the skip came from an exact-id (closed) match
}

func TestAppendTasksTable(t *testing.T) {
	date := "20260903"
	rows := deployRows(sampleTemplate(), "alpha", date, "grp")
	firstLine := `{"id":"QUALITY-SWEEP-20260903-alpha-01","status":"pending","title":"x"}`
	secondLine := `{"id":"QUALITY-SWEEP-20260903-alpha-02","status":"pending","title":"y"}`
	closedSameDate := `{"id":"QUALITY-SWEEP-20260903-alpha-01","status":"complete","worker_status":"complete","completed_at":"2026-09-03 10:00:00.000000"}`
	openSamePrefix := `{"id":"QUALITY-SWEEP-20260903-alpha-99","status":"pending","title":"manual extra"}`
	otherProject := `{"id":"QUALITY-SWEEP-20260903-beta-01","status":"pending","title":"other"}`

	cases := []boardTestCase{
		{
			name:       "append to empty board",
			seed:       "",
			rows:       rows,
			wantAppend: 2,
			wantLines:  2,
		},
		{
			name:       "append after unrelated rows",
			seed:       otherProject + "\n",
			rows:       rows,
			wantAppend: 2,
			wantLines:  3,
		},
		{
			name:       "idempotent skip on exact same deploy",
			seed:       firstLine + "\n" + secondLine + "\n",
			rows:       rows,
			wantAppend: 0,
			wantSkip:   true,
			wantLines:  2,
		},
		{
			name:       "skip when closed row carries same exact id (same-date redeploy)",
			seed:       closedSameDate + "\n",
			rows:       rows,
			wantAppend: 0,
			wantSkip:   true,
			wantLines:  1,
		},
		{
			name:       "skip when open row shares template id prefix",
			seed:       openSamePrefix + "\n",
			rows:       rows,
			wantAppend: 0,
			wantSkip:   true,
			wantLines:  1,
		},
		{
			name:       "different date appends again (fresh deployment)",
			seed:       firstLine + "\n",
			rows:       deployRows(sampleTemplate(), "alpha", "20260904", "grp"),
			wantAppend: 2,
			wantLines:  3,
		},
		{
			name:       "torn final line tolerated — append still lands and file stays parseable",
			seed:       otherProject + "\n{\"id\":\"QUALITY-SWEEP-20260903-alpha-01\",\"status\":\"pendin",
			rows:       rows,
			wantAppend: 2,
			wantLines:  4, // unrelated row + torn fragment line + 2 appended rows
		},
		{
			name:       "malformed middle lines skipped, append proceeds",
			seed:       "garbage not json\n" + firstLine + "\nmore garbage\n",
			rows:       deployRows(sampleTemplate(), "beta", "20260903", "grp"),
			wantAppend: 2,
			wantLines:  5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wd, path := makeBoard(t, tc.seed)
			res, err := AppendTasks(wd, tc.rows)
			if !errors.Is(err, tc.wantErrKind) {
				t.Fatalf("AppendTasks err = %v, want %v", err, tc.wantErrKind)
			}
			if tc.wantSkip != res.Skipped {
				t.Fatalf("Skipped = %v, want %v (reason: %s)", res.Skipped, tc.wantSkip, res.SkipReason)
			}
			if len(res.Appended) != tc.wantAppend {
				t.Fatalf("appended %d ids, want %d: %v", len(res.Appended), tc.wantAppend, res.Appended)
			}
			lines := readBoardLines(t, path)
			if len(lines) != tc.wantLines {
				t.Fatalf("board has %d lines, want %d", len(lines), tc.wantLines)
			}
		})
	}
}

func TestAppendTasksMissingWorkdir(t *testing.T) {
	rows := deployRows(sampleTemplate(), "alpha", "20260903", "grp")
	if _, err := AppendTasks("", rows); !errors.Is(err, ErrWorkdirMissing) {
		t.Fatalf("empty workdir err = %v, want ErrWorkdirMissing", err)
	}
	if _, err := AppendTasks("/nonexistent/nowhere-xyz", rows); !errors.Is(err, ErrWorkdirMissing) {
		t.Fatalf("missing workdir err = %v, want ErrWorkdirMissing", err)
	}
}

func TestAppendTasksNoBoard(t *testing.T) {
	rows := deployRows(sampleTemplate(), "alpha", "20260903", "grp")
	wd := t.TempDir() // exists, but no .coding-hermes/board/tasks.jsonl
	if _, err := AppendTasks(wd, rows); !errors.Is(err, ErrNoBoard) {
		t.Fatalf("no-board err = %v, want ErrNoBoard", err)
	}
}

func TestPlanAppendIsReadOnly(t *testing.T) {
	// Dry-run planning must never create or mutate the board.
	wd := t.TempDir() // no board at all
	rows := deployRows(sampleTemplate(), "alpha", "20260903", "grp")
	skipped, _, err := planAppend(wd, rows)
	if err == nil || skipped {
		t.Fatalf("planAppend on boardless workdir = skipped=%v err=%v, want error", skipped, err)
	}
	if _, statErr := os.Stat(filepath.Join(wd, ".coding-hermes")); !os.IsNotExist(statErr) {
		t.Fatalf("planAppend created files: %v", statErr)
	}

	// On a real board with an existing deployment it reports would-skip
	// without writing anything.
	firstLine := `{"id":"QUALITY-SWEEP-20260903-alpha-01","status":"pending","title":"x"}`
	wd2, path := makeBoard(t, firstLine+"\n")
	skipped, reason, err := planAppend(wd2, rows)
	if err != nil {
		t.Fatalf("planAppend err: %v", err)
	}
	if !skipped || !strings.Contains(reason, "already exists") {
		t.Fatalf("planAppend skipped=%v reason=%q", skipped, reason)
	}
	lines := readBoardLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("planAppend mutated board: %d lines, want 1", len(lines))
	}
}

func TestTaskIDSubstitution(t *testing.T) {
	id, prefix := taskID("{TEMPLATE}-{DATE}-{PROJECT}-{TASK}", "TPL", "my-proj", "20260903", 1)
	if id != "TPL-20260903-my-proj-01" {
		t.Errorf("default pattern id = %q", id)
	}
	if prefix != "TPL-20260903-my-proj" {
		t.Errorf("default pattern prefix = %q", prefix)
	}
	// {N} alias works.
	id, _ = taskID("CLEAN-{N}", "TPL", "p", "20260903", 7)
	if id != "CLEAN-07" {
		t.Errorf("{N} id = %q, want CLEAN-07", id)
	}
	// Pattern without task ordinal gets -{TASK} appended (uniqueness).
	id, prefix = taskID("ONESHOT-{DATE}-{PROJECT}", "TPL", "p", "20260903", 1)
	if id != "ONESHOT-20260903-p-01" {
		t.Errorf("no-ordinal id = %q, want ONESHOT-20260903-p-01", id)
	}
	if prefix != "ONESHOT-20260903-p" {
		t.Errorf("no-ordinal prefix = %q, want ONESHOT-20260903-p", prefix)
	}
}
