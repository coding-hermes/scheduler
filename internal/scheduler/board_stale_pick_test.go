package scheduler

// SCHED-GAP-16981 tests: pick-time BOARD-STALE drift evidence.
//
// The fixture is a real git checkout (work/ + a bare origin/) because the
// AC's comparison is byte-exact against origin/main through git — a fake
// differ would prove nothing. Every git invocation in the fixture runs with
// GIT_CONFIG_GLOBAL/SYSTEM=/dev/null: the host's global core.hooksPath
// (/home/kara/.hermes/hooks) must never fire on a test's `git commit`, and
// the test must not inherit the host's identity either.

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"database/sql"

	_ "modernc.org/sqlite"
)

// boardStaleGit runs one git command in dir with the host config isolated.
func boardStaleGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s in %s failed: %v\nstderr: %s", strings.Join(args, " "), dir, err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

// boardStaleFixture builds a bare origin repo and a work checkout whose
// tasks.jsonl holds one row per (id, status), committed to origin/main,
// then applies localDelta as a second (unpushed) commit. It returns the
// workdir and writes the board at .coding-hermes/board/tasks.jsonl — the
// path findBoardFile resolves.
func boardStaleFixture(t *testing.T, baseRows string, localDelta string) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")

	boardDir := filepath.Join(work, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	boardPath := filepath.Join(boardDir, "tasks.jsonl")
	writeBoardStaleBoard(t, boardPath, baseRows)

	if err := exec.Command("git", "init", "--bare", "-b", "main", origin).Run(); err != nil {
		// Older git without -b: init bare and point HEAD at main by hand.
		boardStaleGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	}
	boardStaleGit(t, work, "init", "-b", "main", ".")
	boardStaleGit(t, work, "remote", "add", "origin", origin)
	boardStaleGit(t, work, "add", ".")
	boardStaleGit(t, work, "-c", "user.name=fixture", "-c", "user.email=fixture@test",
		"commit", "-m", "board: base rows")
	boardStaleGit(t, work, "push", "origin", "main")
	if localDelta != "" {
		writeBoardStaleBoard(t, boardPath, localDelta)
		boardStaleGit(t, work, "add", ".")
		boardStaleGit(t, work, "-c", "user.name=fixture", "-c", "user.email=fixture@test",
			"commit", "-m", "board: local delta (unpushed)")
	}
	return work
}

func writeBoardStaleBoard(t *testing.T, path, rows string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(rows), 0o644); err != nil {
		t.Fatalf("write board: %v", err)
	}
}

// boardStaleCaptureLog redirects the standard logger into a buffer (the
// admitCaptureLog pattern; the package's tests run under -p 1).
func boardStaleCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// TestBoardStalePickEmitsDriftLine is the AC's positive arm: a picked
// board-driven lane whose checkout board differs from origin/main gets ONE
// line naming workdir sha, origin sha and the disagreeing id — and when
// the bytes match again, no further line.
func TestBoardStalePickEmitsDriftLine(t *testing.T) {
	base := "{\"id\":\"CF-079\",\"status\":\"pending\"}\n{\"id\":\"CF-080\",\"status\":\"pending\"}\n"
	delta := "{\"id\":\"CF-079\",\"status\":\"complete\"}\n{\"id\":\"CF-080\",\"status\":\"pending\"}\n"
	work := boardStaleFixture(t, base, delta)

	headSHA := boardStaleGit(t, work, "rev-parse", "--verify", "HEAD^{commit}")
	originSHA := boardStaleGit(t, work, "rev-parse", "--verify", "origin/main^{commit}")
	if headSHA == originSHA {
		t.Fatalf("fixture broken: HEAD == origin/main")
	}

	buf := boardStaleCaptureLog(t)
	logBoardStaleForLane("code-factory", work)

	lines := boardStaleLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("want exactly 1 BOARD-STALE line, got %d:\n%s", len(lines), buf.String())
	}
	line := lines[0]
	if !strings.Contains(line, "workdir_sha="+headSHA) {
		t.Errorf("line missing workdir sha %s: %s", headSHA, line)
	}
	if !strings.Contains(line, "origin_main_sha="+originSHA) {
		t.Errorf("line missing origin sha %s: %s", originSHA, line)
	}
	if !strings.Contains(line, "CF-079:complete->pending") {
		t.Errorf("line missing disagreeing id CF-079:complete->pending: %s", line)
	}
	if strings.Contains(line, "CF-080") {
		t.Errorf("CF-080 agrees in both copies — must not be listed: %s", line)
	}
	t.Logf("BOARD-STALE line: %s", line)

	// Negative arm: restore the workdir board to origin/main's exact bytes
	// → the comparison must stay silent (no new BOARD-STALE line).
	originBytes := boardStaleGit(t, work, "cat-file", "blob", "origin/main:.coding-hermes/board/tasks.jsonl")
	writeBoardStaleBoard(t, filepath.Join(work, ".coding-hermes", "board", "tasks.jsonl"), originBytes+"\n")
	buf.Reset()
	logBoardStaleForLane("code-factory", work)
	if got := boardStaleLines(buf.String()); len(got) != 0 {
		t.Fatalf("bytes match origin/main — BOARD-STALE must stay silent, got:\n%s", got[0])
	}
}

// TestBoardStalePickDirtyCopyStillEmits pins that the AC's trigger is the
// BOARD BYTES, not the shas: a dirty uncommitted board on top of a HEAD
// equal to origin/main is still drift, and the line still names both shas.
func TestBoardStalePickDirtyCopyStillEmits(t *testing.T) {
	base := "{\"id\":\"TR-9\",\"status\":\"pending\"}\n"
	work := boardStaleFixture(t, base, "") // HEAD == origin/main

	// Dirty the working copy WITHOUT committing.
	boardPath := filepath.Join(work, ".coding-hermes", "board", "tasks.jsonl")
	writeBoardStaleBoard(t, boardPath, "{\"id\":\"TR-9\",\"status\":\"complete\"}\n")

	headSHA := boardStaleGit(t, work, "rev-parse", "--verify", "HEAD^{commit}")
	originSHA := boardStaleGit(t, work, "rev-parse", "--verify", "origin/main^{commit}")
	if headSHA != originSHA {
		t.Fatalf("fixture broken: HEAD != origin/main")
	}

	buf := boardStaleCaptureLog(t)
	logBoardStaleForLane("tr", work)
	lines := boardStaleLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("dirty board copy must emit exactly 1 line, got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "TR-9:complete->pending") {
		t.Errorf("line missing dirty-copy disagreement: %s", lines[0])
	}
	t.Logf("dirty-copy BOARD-STALE line: %s", lines[0])
}

// TestBoardStalePickSkipReasons covers the fail-safe arms: a non-git
// checkout and a git checkout without a resolvable origin/main each log
// exactly ONE reason-coded skip (deduped across picks), and the calls never
// panic or fail.
func TestBoardStalePickSkipReasons(t *testing.T) {
	// Arm 1: not a git repo.
	bare := t.TempDir()
	boardDir := filepath.Join(bare, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeBoardStaleBoard(t, filepath.Join(boardDir, "tasks.jsonl"),
		"{\"id\":\"X-1\",\"status\":\"pending\"}\n")

	buf := boardStaleCaptureLog(t)
	logBoardStaleForLane("lone", bare)
	logBoardStaleForLane("lone", bare) // second pick: still ONE skip
	skips := boardStaleSkipLines(buf.String())
	if len(skips) != 1 {
		t.Fatalf("non-git checkout: want exactly 1 deduped skip, got %d:\n%s", len(skips), buf.String())
	}
	if !strings.Contains(skips[0], "reason=not-a-git-repo") {
		t.Errorf("skip missing reason=not-a-git-repo: %s", skips[0])
	}

	// Arm 2: a git repo with no origin/main (no remote configured).
	noremote := t.TempDir()
	boardDir2 := filepath.Join(noremote, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeBoardStaleBoard(t, filepath.Join(boardDir2, "tasks.jsonl"),
		"{\"id\":\"X-1\",\"status\":\"pending\"}\n")
	boardStaleGit(t, noremote, "init", "-b", "main", ".")
	boardStaleGit(t, noremote, "add", ".")
	boardStaleGit(t, noremote, "-c", "user.name=fixture", "-c", "user.email=fixture@test",
		"commit", "-m", "board: no remote here")
	buf.Reset()
	logBoardStaleForLane("lone", noremote)
	logBoardStaleForLane("lone", noremote)
	skips = boardStaleSkipLines(buf.String())
	if len(skips) != 1 {
		t.Fatalf("no-origin checkout: want exactly 1 deduped skip, got %d:\n%s", len(skips), buf.String())
	}
	if !strings.Contains(skips[0], "reason=no-origin-main") {
		t.Errorf("skip missing reason=no-origin-main: %s", skips[0])
	}
	t.Logf("skip line: %s", skips[0])
}

// TestBoardStaleDiffEntriesCap pins the bounded list: 12 disagreements
// render as 8 entries plus "+4 more" on the same single line.
func TestBoardStaleDiffEntriesCap(t *testing.T) {
	var local, origin strings.Builder
	for i := 0; i < 12; i++ {
		fmt.Fprintf(&local, "{\"id\":\"CF-%03d\",\"status\":\"pending\"}\n", i)
		fmt.Fprintf(&origin, "{\"id\":\"CF-%03d\",\"status\":\"complete\"}\n", i)
	}
	entries := boardStaleDiffEntries(
		jsonlBoardStatusIndex([]byte(local.String())),
		jsonlBoardStatusIndex([]byte(origin.String())))
	if len(entries) != boardStaleMaxEntries+1 {
		t.Fatalf("want %d capped entries (+N more), got %d", boardStaleMaxEntries+1, len(entries))
	}
	if entries[len(entries)-1] != "+4 more" {
		t.Errorf("cap marker want +4 more, got %q", entries[len(entries)-1])
	}
}

// TestBoardStaleAtPickTasksModeOnly proves the loop-level hook checks
// board-driven (tasks) lanes only: a drifted tasks lane emits, a drifted
// cooldown lane sharing the same checkout stays silent, and the packed
// list is not mutated (logging only).
func TestBoardStaleAtPickTasksModeOnly(t *testing.T) {
	base := "{\"id\":\"CF-079\",\"status\":\"pending\"}\n"
	delta := "{\"id\":\"CF-079\",\"status\":\"complete\"}\n"
	work := boardStaleFixture(t, base, delta)

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	// Minimal schema for admissionModeForProject (project override wins).
	_, err = db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY, admission_mode TEXT, namespace_id TEXT);
		CREATE TABLE namespaces (id TEXT PRIMARY KEY, admission_mode TEXT)`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, p := range []struct{ name, mode string }{
		{"code-factory-tasks", "tasks"},
		{"code-factory-cool", "cooldown"},
	} {
		if _, err := db.Exec(`INSERT INTO projects (name, admission_mode) VALUES (?, ?)`, p.name, p.mode); err != nil {
			t.Fatalf("seed %s: %v", p.name, err)
		}
	}

	packed := []PackedProject{
		{Name: "code-factory-tasks", Workdir: work},
		{Name: "code-factory-cool", Workdir: work},
	}
	buf := boardStaleCaptureLog(t)
	logBoardStaleAtPick(db, packed)

	lines := boardStaleLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("tasks-mode pick must emit exactly 1 line (cooldown lane silent), got %d:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], "code-factory-tasks") {
		t.Errorf("line is not the tasks lane's: %s", lines[0])
	}
	if packed[0].Name != "code-factory-tasks" || packed[1].Workdir != work {
		t.Errorf("packed list mutated by the logging hook")
	}
	t.Logf("hook BOARD-STALE line: %s", lines[0])
}

// boardStaleLines extracts the BOARD-STALE evidence lines from a capture.
func boardStaleLines(captured string) []string {
	return boardStaleGrep(captured, "BOARD-STALE (SCHED-GAP-16981):")
}

// boardStaleSkipLines extracts the reason-coded skip lines.
func boardStaleSkipLines(captured string) []string {
	return boardStaleGrep(captured, "BOARD-STALE-SKIP (SCHED-GAP-16981):")
}

func boardStaleGrep(captured, token string) []string {
	var out []string
	for _, ln := range strings.Split(captured, "\n") {
		if strings.Contains(ln, token) {
			out = append(out, ln)
		}
	}
	return out
}
