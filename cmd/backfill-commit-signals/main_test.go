package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// =============================================================================
// Helpers — every test runs against a temp sqlite (never the live DB) and a
// temp git repo, and passes --no-board (or an explicit temp board) so a test
// run can never discover and rewrite this repository's real board.
// =============================================================================

func newTestDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scheduler.db")
	db, err := database.InitDB(path)
	if err != nil {
		t.Fatalf("InitDB(%s): %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func insertProject(t *testing.T, db *sql.DB, name, workdir string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO projects (name, repo_url, workdir, created_at, updated_at)
		VALUES (?, '', ?, datetime('now'), datetime('now'))`, name, workdir); err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

type tickSeed struct {
	id, project, spawned, completed string
	commits                         int
	code, board                     int
	files                           int
	cost                            float64
	errMsg                          string
}

func insertTick(t *testing.T, db *sql.DB, tk tickSeed) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO ticks
		(id, project_name, status, spawned_at, completed_at, commits, files_changed, cost_usd, error, code_commits, board_commits, created_at)
		VALUES (?, ?, 'completed', ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		tk.id, tk.project, tk.spawned, tk.completed, tk.commits, tk.files, tk.cost, tk.errMsg, tk.code, tk.board); err != nil {
		t.Fatalf("insert tick %s: %v", tk.id, err)
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitRunAt runs git with both author and committer dates pinned to `when`, so
// a test can place commits in distinct seconds and drive the bounded-window
// reconstruction deterministically (git timestamps have 1s resolution).
func gitRunAt(t *testing.T, dir string, when time.Time, args ...string) {
	t.Helper()
	stamp := when.Format(time.RFC3339)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepo creates a git repo with one empty baseline commit at `base` (no
// paths, so the classifier never counts it).
func initRepo(t *testing.T, base time.Time) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init")
	gitRun(t, dir, "config", "user.email", "t@example.com")
	gitRun(t, dir, "config", "user.name", "t")
	gitRunAt(t, dir, base, "commit", "-m", "baseline", "--allow-empty")
	return dir
}

// commitPaths writes each path and commits exactly those paths at `when`.
func commitPaths(t *testing.T, dir string, when time.Time, rels []string, msg string) {
	t.Helper()
	args := make([]string, 3, 3+len(rels))
	args[0] = "-C"
	args[1] = dir
	args[2] = "add"
	for _, rel := range rels {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte("x\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		args = append(args, rel)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	gitRunAt(t, dir, when, "commit", "-m", msg, "--allow-empty")
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// readTicks renders every ticks row (all columns) as text, so a test can prove
// a dry-run wrote nothing and an apply touched only the two anatomy columns.
func readTicks(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM ticks ORDER BY id`)
	if err != nil {
		t.Fatalf("select ticks: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		_, _ = fmt.Fprintf(&b, "%v\n", vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return b.String()
}

// readTickRow returns one tick row as a column→text map.
func readTickRow(t *testing.T, db *sql.DB, id string) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT * FROM ticks WHERE id = ?`, id)
	if err != nil {
		t.Fatalf("select tick %s: %v", id, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("tick %s not found", id)
	}
	vals := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan %s: %v", id, err)
	}
	out := map[string]string{}
	for i, c := range cols {
		out[c] = fmt.Sprintf("%v", vals[i])
	}
	return out
}

func readAnatomy(t *testing.T, db *sql.DB, id string) (code, board int) {
	t.Helper()
	if err := db.QueryRow(`SELECT code_commits, board_commits FROM ticks WHERE id = ?`, id).
		Scan(&code, &board); err != nil {
		t.Fatalf("read anatomy %s: %v", id, err)
	}
	return code, board
}

// fixture seeds the standard ticks used by the run tests. The repo carries
// commits at distinct, pinned instants so the bounded reconstruction is
// exercised exactly:
//
//	base+0m    baseline (empty)
//	base+1m    a.go        } inside T-CODE's window
//	base+2m    b.go        }
//	base+3m    board row   }
//	base+20m   later.go    } AFTER every tick — must NOT be attributed to them
//	base+21m   later2.go   }
//
//	T-CODE      back-fillable: window split = 2 code + 1 board (claims 3)
//	T-GONE      unrecoverable (workdir no longer on disk)
//	T-NOCOMMITS matches no predicate (commits = 0)
//	T-OLD       outside the time window
//	T-STAMPED   already measured (code_commits = 5)
//	T-NOEND     no completed_at -> falls back to the open-ended window
func fixture(t *testing.T, db *sql.DB, now time.Time) (since, until string) {
	t.Helper()
	base := now.Add(-30 * time.Minute)
	repo := initRepo(t, base)
	commitPaths(t, repo, base.Add(1*time.Minute), []string{"a.go"}, "feat: a")
	commitPaths(t, repo, base.Add(2*time.Minute), []string{"b.go"}, "feat: b")
	commitPaths(t, repo, base.Add(3*time.Minute), []string{".coding-hermes/board/tasks.jsonl"}, "chore(board): tick")
	commitPaths(t, repo, base.Add(20*time.Minute), []string{"later.go"}, "feat: later tick")
	commitPaths(t, repo, base.Add(21*time.Minute), []string{"later2.go"}, "feat: later tick 2")

	insertProject(t, db, "code-proj", repo)
	insertProject(t, db, "gone-proj", filepath.Join(t.TempDir(), "removed-workdir"))

	spawned := base.Add(30 * time.Second).Format(time.RFC3339)
	completed := base.Add(4 * time.Minute).Format(time.RFC3339)
	insertTick(t, db, tickSeed{id: "T-CODE", project: "code-proj", spawned: spawned, completed: completed, commits: 3, files: 11, cost: 1.25, errMsg: "keep-me"})
	insertTick(t, db, tickSeed{id: "T-GONE", project: "gone-proj", spawned: spawned, completed: completed, commits: 2, files: 4})
	insertTick(t, db, tickSeed{id: "T-NOCOMMITS", project: "code-proj", spawned: spawned, completed: completed, commits: 0})
	insertTick(t, db, tickSeed{id: "T-OLD", project: "code-proj", spawned: base.Add(-48 * time.Hour).Format(time.RFC3339), completed: completed, commits: 3})
	insertTick(t, db, tickSeed{id: "T-STAMPED", project: "code-proj", spawned: spawned, completed: completed, commits: 3, code: 5, board: 7})
	insertTick(t, db, tickSeed{id: "T-NOEND", project: "code-proj", spawned: spawned, commits: 3})

	return now.Add(-60 * time.Minute).Format(time.RFC3339), now.Add(10 * time.Minute).Format(time.RFC3339)
}

// =============================================================================
// Dry-run: reports the row's acceptance counts and writes nothing.
// =============================================================================

func TestRun_DryRunReportsCountsAndWritesNothing(t *testing.T) {
	db, dbPath := newTestDB(t)
	now := time.Now()
	since, until := fixture(t, db, now)

	before := readTicks(t, db)
	code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--no-board")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	const want = "3 affected / 2 back-fillable / 1 unrecoverable"
	if !strings.Contains(out, want) {
		t.Errorf("dry-run output missing %q:\n%s", want, out)
	}
	if !strings.Contains(out, "DRY-RUN") {
		t.Errorf("dry-run did not label the mode:\n%s", out)
	}
	// The window reconstruction is reported, not silently assumed.
	if !strings.Contains(out, "window     : 1 of 2 back-fillable ticks measured inside their own window") {
		t.Errorf("dry-run did not report the window reconstruction:\n%s", out)
	}
	// The unrecoverable tick is named with its cause.
	if !strings.Contains(out, "T-GONE") || !strings.Contains(out, "gone-proj") || !strings.Contains(out, "workdir missing on disk") {
		t.Errorf("dry-run did not name the unrecoverable tick + cause:\n%s", out)
	}
	if got := readTicks(t, db); got != before {
		t.Errorf("dry-run mutated the database\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if _, board := readAnatomy(t, db, "T-CODE"); board != 0 {
		t.Error("dry-run stamped T-CODE")
	}
}

// =============================================================================
// Apply: stamps ONLY the affected ticks, and only their two anatomy columns.
// =============================================================================

func TestRun_ApplyStampsOnlyAffectedRowsAndColumns(t *testing.T) {
	db, dbPath := newTestDB(t)
	now := time.Now()
	since, until := fixture(t, db, now)

	preCode := readTickRow(t, db, "T-CODE")
	preNoCommits := readTickRow(t, db, "T-NOCOMMITS")
	preOld := readTickRow(t, db, "T-OLD")
	preStamped := readTickRow(t, db, "T-STAMPED")

	code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--apply", "--no-board")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.Contains(out, "3 affected / 2 back-fillable / 1 unrecoverable") {
		t.Errorf("apply output lost the count line:\n%s", out)
	}
	if !strings.Contains(out, "applied: 3 tick row(s) updated (2 stamped, 1 marked -1/-1)") {
		t.Errorf("apply output did not report the write count:\n%s", out)
	}

	t.Run("back-fillable tick gets the split for ITS OWN window", func(t *testing.T) {
		gotCode, gotBoard := readAnatomy(t, db, "T-CODE")
		if gotCode != 2 || gotBoard != 1 {
			t.Errorf("T-CODE anatomy = (%d, %d), want (2, 1) — the 2 code + 1 board commits inside spawned_at..completed_at; the two later commits belong to a later tick", gotCode, gotBoard)
		}
	})

	t.Run("a tick with no completed_at falls back to the open-ended window", func(t *testing.T) {
		gotCode, gotBoard := readAnatomy(t, db, "T-NOEND")
		if gotCode != 4 || gotBoard != 1 {
			t.Errorf("T-NOEND anatomy = (%d, %d), want (4, 1) — no completed_at means the unbounded measurement, reported as an upper bound", gotCode, gotBoard)
		}
	})

	t.Run("unrecoverable tick gets the explicit unmeasured marker", func(t *testing.T) {
		gotCode, gotBoard := readAnatomy(t, db, "T-GONE")
		if gotCode != -1 || gotBoard != -1 {
			t.Errorf("T-GONE anatomy = (%d, %d), want (-1, -1)", gotCode, gotBoard)
		}
	})

	t.Run("no other column of a written row changed", func(t *testing.T) {
		post := readTickRow(t, db, "T-CODE")
		for k, v := range preCode {
			if k == "code_commits" || k == "board_commits" {
				continue
			}
			if post[k] != v {
				t.Errorf("column %s changed: %q -> %q", k, v, post[k])
			}
		}
	})

	t.Run("rows outside the predicate are untouched", func(t *testing.T) {
		for id, pre := range map[string]map[string]string{
			"T-NOCOMMITS": preNoCommits,
			"T-OLD":       preOld,
			"T-STAMPED":   preStamped,
		} {
			post := readTickRow(t, db, id)
			for k, v := range pre {
				if post[k] != v {
					t.Errorf("%s column %s changed: %q -> %q", id, k, v, post[k])
				}
			}
		}
	})

	t.Run("re-run is a no-op", func(t *testing.T) {
		after := readTicks(t, db)
		code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--apply", "--no-board")
		if code != 0 {
			t.Fatalf("second apply exit %d, stderr:\n%s", code, errOut)
		}
		if !strings.Contains(out, "0 affected / 0 back-fillable / 0 unrecoverable") {
			t.Errorf("second apply re-selected ticks:\n%s", out)
		}
		if got := readTicks(t, db); got != after {
			t.Errorf("second apply changed the database\nbefore:\n%s\nafter:\n%s", after, got)
		}
	})
}

// TestRun_ApplyDryRunConflictWritesNothing pins the safety resolution of
// `--apply --dry-run=true`: the explicit dry-run wins.
func TestRun_ApplyDryRunConflictWritesNothing(t *testing.T) {
	db, dbPath := newTestDB(t)
	now := time.Now()
	since, until := fixture(t, db, now)

	before := readTicks(t, db)
	code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--apply", "--dry-run=true", "--no-board")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, errOut)
	}
	if !strings.Contains(out, "DRY-RUN") {
		t.Errorf("explicit --dry-run=true did not win over --apply:\n%s", out)
	}
	if got := readTicks(t, db); got != before {
		t.Error("--apply --dry-run=true mutated the database")
	}
}

// TestApplyDispositions_SkipsRowsAlreadyStamped covers the concurrency guard:
// a row the daemon stamped while the walk was in flight is never clobbered.
func TestApplyDispositions_SkipsRowsAlreadyStamped(t *testing.T) {
	db, _ := newTestDB(t)
	now := time.Now()
	insertProject(t, db, "p", t.TempDir())
	spawned := now.Add(-2 * time.Minute).Format(time.RFC3339)
	insertTick(t, db, tickSeed{id: "T-RACE", project: "p", spawned: spawned, commits: 3})

	ds := []disposition{{
		Tick:  affectedTick{ID: "T-RACE", Project: "p", Claimed: 3},
		Code:  2,
		Board: 1,
		OK:    true,
	}}
	applied, skipped, err := applyDispositions(db, ds)
	if err != nil {
		t.Fatalf("applyDispositions: %v", err)
	}
	if applied != 1 || skipped != 0 {
		t.Fatalf("first apply = (%d applied, %d skipped), want (1, 0)", applied, skipped)
	}
	// Second pass with a stale verdict: the guard refuses the overwrite.
	applied, skipped, err = applyDispositions(db, ds)
	if err != nil {
		t.Fatalf("applyDispositions (second): %v", err)
	}
	if applied != 0 || skipped != 1 {
		t.Errorf("second apply = (%d applied, %d skipped), want (0, 1) — an already-stamped row must not be rewritten", applied, skipped)
	}
	if code, board := readAnatomy(t, db, "T-RACE"); code != 2 || board != 1 {
		t.Errorf("T-RACE anatomy = (%d, %d), want the first split (2, 1)", code, board)
	}
}

// =============================================================================
// Window / flag validation.
// =============================================================================

func TestRun_RejectsBadInvocation(t *testing.T) {
	_, dbPath := newTestDB(t)
	cases := []struct {
		name string
		args []string
	}{
		{"unparseable --since", []string{"--db", dbPath, "--since", "not-a-time"}},
		{"unparseable --until", []string{"--db", dbPath, "--until", "not-a-time"}},
		{"until before since", []string{"--db", dbPath, "--since", "2026-09-21T00:00:00-05:00", "--until", "2026-09-19T00:00:00-05:00"}},
		{"stray positional argument", []string{"--db", dbPath, "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := runCLI(t, tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (usage error)", code)
			}
		})
	}
}

// =============================================================================
// Board review_notes write — the unrecoverable census, and its idempotency.
// =============================================================================

func TestRun_ApplyAppendsBoardReviewNoteForUnrecoverableTicks(t *testing.T) {
	db, dbPath := newTestDB(t)
	now := time.Now()
	since, until := fixture(t, db, now)

	board := filepath.Join(t.TempDir(), "tasks.jsonl")
	const otherRow = `{"id":"OTHER","status":"pending","review_notes":null,"title":"untouched"}`
	const targetRow = `{"id":"SCHED-PERF-001-B","status":"pending","review_notes":null,"title":"back-fill"}`
	if err := os.WriteFile(board, []byte(otherRow+"\n"+targetRow+"\n"), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}

	t.Run("dry-run leaves the board untouched", func(t *testing.T) {
		before, _ := os.ReadFile(board)
		code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--board", board)
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, errOut)
		}
		after, _ := os.ReadFile(board)
		if !bytes.Equal(before, after) {
			t.Error("dry-run rewrote the board")
		}
		if !strings.Contains(out, "board note text that --apply would append") {
			t.Errorf("dry-run did not preview the note:\n%s", out)
		}
	})

	t.Run("apply appends a census naming every unrecoverable tick and its cause", func(t *testing.T) {
		code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--apply", "--board", board, "--board-row", "SCHED-PERF-001-B")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, errOut)
		}
		if !strings.Contains(out, "board: appended review_notes entry") {
			t.Errorf("apply did not report the board write:\n%s", out)
		}
		lines := readBoardLines(t, board)
		if len(lines) != 2 {
			t.Fatalf("board now has %d lines, want 2 (no row added)", len(lines))
		}
		if lines[0] != otherRow {
			t.Errorf("the neighbouring row was rewritten:\n%s", lines[0])
		}
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(lines[1]), &row); err != nil {
			t.Fatalf("target row is no longer valid JSON: %v\n%s", err, lines[1])
		}
		if row["status"] != "pending" || row["title"] != "back-fill" {
			t.Errorf("target row's other fields changed: %v", row)
		}
		note, _ := row["review_notes"].(string)
		for _, want := range []string{boardNoteMarker, "T-GONE", "gone-proj", "workdir missing on disk", "code_commits=-1/board_commits=-1"} {
			if !strings.Contains(note, want) {
				t.Errorf("review_notes missing %q:\n%s", want, note)
			}
		}
	})

	t.Run("second apply does not append twice", func(t *testing.T) {
		before, _ := os.ReadFile(board)
		code, out, errOut := runCLI(t, "--db", dbPath, "--since", since, "--until", until, "--apply", "--board", board, "--board-row", "SCHED-PERF-001-B")
		if code != 0 {
			t.Fatalf("exit %d, stderr:\n%s", code, errOut)
		}
		// The window is exhausted by the first apply, so a re-run reports
		// nothing to do and never reaches the board write.
		if !strings.Contains(out, "0 affected") {
			t.Errorf("re-run re-selected ticks:\n%s", out)
		}
		after, _ := os.ReadFile(board)
		if !bytes.Equal(before, after) {
			t.Error("re-run appended a second review_notes entry")
		}
	})
}

func readBoardLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read board: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestAppendBoardReviewNote_AppendsOntoExistingNote(t *testing.T) {
	dir := t.TempDir()
	board := filepath.Join(dir, "tasks.jsonl")
	const row = `{"id":"R","status":"pending","review_notes":"an earlier reviewer's note","title":"t"}`
	if err := os.WriteFile(board, []byte(row+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err := appendBoardReviewNote(board, "R", "back-fill census")
	if err != nil || !wrote {
		t.Fatalf("appendBoardReviewNote = (%v, %v), want (true, nil)", wrote, err)
	}
	lines := readBoardLines(t, board)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("row is not valid JSON: %v\n%s", err, lines[0])
	}
	if got["review_notes"] != "an earlier reviewer's note || back-fill census" {
		t.Errorf("review_notes = %q, want the previous note preserved and the new one appended", got["review_notes"])
	}
	if got["title"] != "t" || got["status"] != "pending" {
		t.Errorf("row fields changed: %v", got)
	}
}

func TestAppendBoardReviewNote_UnknownRowIsAnError(t *testing.T) {
	board := filepath.Join(t.TempDir(), "tasks.jsonl")
	if err := os.WriteFile(board, []byte(`{"id":"A","review_notes":null}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := appendBoardReviewNote(board, "MISSING", "n"); err == nil {
		t.Error("appending to a row that does not exist returned nil error")
	}
}

func TestSpliceReviewNotes_PreservesEveryOtherByte(t *testing.T) {
	cases := []struct {
		name, line, value, want string
		wantErr                 bool
	}{
		{
			name:  "null value",
			line:  `{"id":"a","review_notes":null,"title":"t"}`,
			value: "note",
			want:  `{"id":"a","review_notes":"note","title":"t"}`,
		},
		{
			name:  "existing string with escapes",
			line:  `{"id":"a","review_notes":"he said \"hi\" \u2192 done","title":"t"}`,
			value: "new",
			want:  `{"id":"a","review_notes":"new","title":"t"}`,
		},
		{
			name:  "whitespace after the key is preserved",
			line:  `{"id":"a","review_notes":  "old" ,"title":"t"}`,
			value: "new",
			want:  `{"id":"a","review_notes":  "new" ,"title":"t"}`,
		},
		{
			name:    "missing key",
			line:    `{"id":"a"}`,
			value:   "new",
			wantErr: true,
		},
		{
			name:    "non-string, non-null value",
			line:    `{"id":"a","review_notes":7}`,
			value:   "new",
			wantErr: true,
		},
		{
			name:    "unterminated string",
			line:    `{"id":"a","review_notes":"oops}`,
			value:   "new",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spliceReviewNotes(tc.line, tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("spliceReviewNotes: %v", err)
			}
			if got != tc.want {
				t.Errorf("splice = %q, want %q", got, tc.want)
			}
			var probe map[string]interface{}
			if err := json.Unmarshal([]byte(got), &probe); err != nil {
				t.Errorf("spliced line is not valid JSON: %v", err)
			}
		})
	}
}

// TestResolveBoardPath_NoGitRootIsEmpty keeps the auto-discovery honest: with
// no repo to discover, the tool must not invent a board path.
func TestResolveBoardPath_NoGitRootIsEmpty(t *testing.T) {
	dir := t.TempDir() // not a git repo — and on macOS /tmp is not climbed into a parent repo
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if got := resolveBoardPath(""); got != "" {
		t.Errorf("resolveBoardPath(\"\") = %q in a non-repo dir, want \"\"", got)
	}
}

// =============================================================================
// Cause attribution — the census must group by the right reason.
// =============================================================================

// TestClassifyTick_UnderReportCountsOnlyCommitsWithPaths pins the diagnostic's
// counting to the classifier's: a commit that touched no path (empty/merge) is
// not reconciled by the classifier, so a tick that claimed 3 commits against a
// history with 2 path-touching commits + 1 empty commit must be attributed to
// the under-report class with the number 2 — counting plain git log lines
// would report 3 and blame the wrong cause.
func TestClassifyTick_UnderReportCountsOnlyCommitsWithPaths(t *testing.T) {
	now := time.Now()
	base := now.Add(-30 * time.Minute)
	repo := initRepo(t, base)
	commitPaths(t, repo, base.Add(1*time.Minute), []string{"a.go"}, "feat: a")
	commitPaths(t, repo, base.Add(2*time.Minute), []string{"b.go"}, "feat: b")
	gitRunAt(t, repo, base.Add(3*time.Minute), "commit", "-m", "chore: empty", "--allow-empty")

	d := classifyTick(affectedTick{
		ID:        "T-UNDER",
		Project:   "p",
		Workdir:   repo,
		SpawnedAt: base.Add(30 * time.Second).Format(time.RFC3339),
		Started:   base.Add(30 * time.Second),
		Claimed:   3,
	})
	if d.OK {
		t.Fatal("classifier accepted a history that under-reports the tick's claim")
	}
	if d.CauseClass != causeGitUnderReports {
		t.Errorf("cause class = %q, want %q (cause: %s)", d.CauseClass, causeGitUnderReports, d.Cause)
	}
	if !strings.Contains(d.Cause, "git sees 2 commit(s) touching files") {
		t.Errorf("cause did not report the path-aware count (want 2, not the 3 raw log lines):\n%s", d.Cause)
	}
}

// TestClassifyTick_BoundsWindowToTheTick is the load-bearing test for the
// reconstruction: commits that belong to LATER ticks in the same workdir must
// not be attributed to this one. The classifier's window is open-ended to HEAD,
// so the tool differences two canonical measurements (since tick start, since
// tick end) — and that difference must be exactly the tick's own split, with the
// open-ended fallback reachable only when completed_at is unusable.
func TestClassifyTick_BoundsWindowToTheTick(t *testing.T) {
	base := time.Now().Add(-30 * time.Minute)
	repo := initRepo(t, base)
	// Inside the tick's window.
	commitPaths(t, repo, base.Add(1*time.Minute), []string{"a.go"}, "feat: a")
	commitPaths(t, repo, base.Add(2*time.Minute), []string{"b.go"}, "feat: b")
	commitPaths(t, repo, base.Add(3*time.Minute), []string{".coding-hermes/board/tasks.jsonl"}, "chore(board): tick")
	// After it — a later tick's work, which must not be attributed here.
	commitPaths(t, repo, base.Add(20*time.Minute), []string{"later.go"}, "feat: later")
	commitPaths(t, repo, base.Add(21*time.Minute), []string{"later2.go"}, "feat: later 2")

	spawnedAt := base.Add(30 * time.Second)
	tick := affectedTick{
		ID:          "T-WINDOW",
		Project:     "p",
		Workdir:     repo,
		Claimed:     3,
		SpawnedAt:   spawnedAt.Format(time.RFC3339),
		CompletedAt: base.Add(4 * time.Minute).Format(time.RFC3339),
		Started:     spawnedAt,
		Completed:   base.Add(4 * time.Minute),
	}

	t.Run("bounded by completed_at", func(t *testing.T) {
		d := classifyTick(tick)
		if !d.OK {
			t.Fatalf("ok = false: %s", d.Cause)
		}
		if !d.Bounded {
			t.Error("Bounded = false although completed_at is usable")
		}
		if d.Code != 2 || d.Board != 1 {
			t.Errorf("split = (%d code, %d board), want (2, 1) — only the commits inside the tick's window", d.Code, d.Board)
		}
		if d.Total != 3 {
			t.Errorf("total = %d, want 3 (the tick's own commits)", d.Total)
		}
		if d.RawTotal != 5 {
			t.Errorf("RawTotal = %d, want 5 (the open-ended window the raw classifier call sees)", d.RawTotal)
		}
	})

	t.Run("no usable completed_at falls back to the open-ended window", func(t *testing.T) {
		unbounded := tick
		unbounded.CompletedAt, unbounded.Completed = "", time.Time{}
		d := classifyTick(unbounded)
		if !d.OK {
			t.Fatalf("ok = false: %s", d.Cause)
		}
		if d.Bounded {
			t.Error("Bounded = true without a completed_at")
		}
		if d.Code != 4 || d.Board != 1 {
			t.Errorf("split = (%d code, %d board), want (4, 1) — the open-ended window is a documented upper bound", d.Code, d.Board)
		}
	})

	t.Run("completed_at before spawned_at is refused, not subtracted", func(t *testing.T) {
		bad := tick
		bad.Completed = tick.Started.Add(-time.Hour)
		d := classifyTick(bad)
		if !d.OK {
			t.Fatalf("ok = false: %s", d.Cause)
		}
		if d.Bounded {
			t.Error("Bounded = true for a completed_at that precedes spawned_at")
		}
		if d.Code != 4 || d.Board != 1 {
			t.Errorf("split = (%d code, %d board), want the open-ended (4, 1)", d.Code, d.Board)
		}
	})
}

// TestCountCommitsWithPathsSinceMirrorsTheClassifier pins the counter the
// bounded reconstruction feeds to the canonical classifier as `claimed`: it must
// count what the classifier counts (blocks with at least one path), so empty
// commits do not inflate the window.
func TestCountCommitsWithPathsSinceMirrorsTheClassifier(t *testing.T) {
	base := time.Now().Add(-30 * time.Minute)
	repo := initRepo(t, base)
	commitPaths(t, repo, base.Add(1*time.Minute), []string{"a.go"}, "feat: a")
	commitPaths(t, repo, base.Add(2*time.Minute), []string{".coding-hermes/board/tasks.jsonl"}, "chore(board): tick")
	gitRunAt(t, repo, base.Add(3*time.Minute), "commit", "-m", "chore: empty", "--allow-empty")
	commitPaths(t, repo, base.Add(4*time.Minute), []string{"c.go", ".coding-hermes/board/events.jsonl"}, "feat: c + board")

	since := base.Add(30 * time.Second)
	got, err := countCommitsWithPathsSince(repo, since)
	if err != nil {
		t.Fatalf("countCommitsWithPathsSince: %v", err)
	}
	if got != 3 {
		t.Fatalf("counted %d commits, want 3 — the empty commit carries no paths and the classifier skips it", got)
	}
	code, board, ok := scheduler.ClassifyGitCommits(repo, since, got)
	if !ok {
		t.Fatal("classifier refused the count this test just produced")
	}
	if code+board != got {
		t.Errorf("classifier total = %d, counter = %d — the mirrored counter must agree with the classifier or the subtraction is unsound", code+board, got)
	}
	if code != 2 || board != 1 {
		t.Errorf("split = (%d, %d), want (2, 1)", code, board)
	}
}

// TestClassifyTick_CauseClasses pins the class each unmeasurable case lands in.
func TestClassifyTick_CauseClasses(t *testing.T) {
	now := time.Now()
	base := affectedTick{ID: "T", Project: "p", SpawnedAt: now.Format(time.RFC3339), Started: now, Claimed: 1}

	cases := []struct {
		name      string
		mutate    func(tk affectedTick) affectedTick
		wantClass string
	}{
		{"unparseable spawned_at", func(tk affectedTick) affectedTick {
			tk.SpawnedAt = "2026-09-20 06:43:46"
			tk.Started = time.Time{}
			return tk
		}, causeSpawnedAtUnparseable},
		{"empty workdir", func(tk affectedTick) affectedTick { tk.Workdir = ""; return tk }, causeWorkdirUnset},
		{"workdir gone from disk", func(tk affectedTick) affectedTick {
			tk.Workdir = filepath.Join(t.TempDir(), "gone")
			return tk
		}, causeWorkdirMissing},
		{"workdir is not a git repo", func(tk affectedTick) affectedTick {
			tk.Workdir = t.TempDir()
			return tk
		}, causeWorkdirMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := classifyTick(tc.mutate(base))
			if d.OK {
				t.Fatal("ok = true for an unmeasurable tick")
			}
			if d.CauseClass != tc.wantClass {
				t.Errorf("cause class = %q, want %q (%s)", d.CauseClass, tc.wantClass, d.Cause)
			}
			if d.Cause == "" {
				t.Error("unmeasurable tick carries no cause text")
			}
		})
	}
}

// TestBuildBoardNote_StampsRunTimeNotWindowStart guards the note's timestamp:
// it must say when the back-fill ran, never the window's start instant.
func TestBuildBoardNote_StampsRunTimeNotWindowStart(t *testing.T) {
	runAt := time.Now()
	windowStart := runAt.Add(-72 * time.Hour)
	res := Result{
		Affected: 2, Backfillable: 1, Unrecoverable: 1, Bounded: 1,
		Dispositions: []disposition{
			{OK: true, Bounded: true, Tick: affectedTick{ID: "T-OK"}},
			{OK: false, CauseClass: causeWorkdirMissing, Cause: "workdir missing on disk: /gone", Tick: affectedTick{ID: "T-BAD", Project: "p"}},
		},
	}
	note := buildBoardNote(res, windowStart.Format(time.RFC3339), runAt.Format(time.RFC3339), runAt)

	idx := strings.Index(note, "(applied ")
	if idx < 0 {
		t.Fatalf("note carries no applied timestamp:\n%s", note)
	}
	rest := note[idx+len("(applied "):]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatalf("applied timestamp is unterminated:\n%s", note)
	}
	stamp, err := time.Parse(time.RFC3339, rest[:end])
	if err != nil {
		t.Fatalf("applied timestamp %q is not RFC3339: %v", rest[:end], err)
	}
	if d := stamp.Sub(runAt); d > time.Minute || d < -time.Minute {
		t.Errorf("applied timestamp = %s, want the run time (~%s)", stamp, runAt)
	}
	if !strings.Contains(note, "T-BAD") || !strings.Contains(note, "workdir missing on disk") {
		t.Errorf("census does not name the unrecoverable tick and its cause:\n%s", note)
	}
}
