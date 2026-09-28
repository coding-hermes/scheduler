package scheduler

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// runGit runs a git command in dir and returns stdout (trimmed).
func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// initTempRepo creates a throwaway git repo with one commit (file a.txt).
func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGitTest(t, dir, "init", "-q")
	runGitTest(t, dir, "config", "user.email", "test@example.com")
	runGitTest(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "add", "a.txt")
	runGitTest(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

func TestGitBaseline(t *testing.T) {
	dir := initTempRepo(t)
	head, total := gitBaseline(dir)
	if head == "" {
		t.Fatalf("expected non-empty HEAD, got %q", head)
	}
	if total != 1 {
		t.Fatalf("expected total=1, got %d", total)
	}

	// Non-git dir → total -1.
	_, nonGit := gitBaseline(t.TempDir())
	if nonGit != -1 {
		t.Fatalf("expected -1 for non-git dir, got %d", nonGit)
	}
}

func TestGitWorkDelta(t *testing.T) {
	dir := initTempRepo(t)
	preHead, preTotal := gitBaseline(dir)
	if preTotal != 1 {
		t.Fatalf("preTotal should be 1, got %d", preTotal)
	}

	// No work → zero delta, and the measurement is KNOWN GOOD (err nil).
	c, f, err := gitWorkDelta(dir, preHead, preTotal)
	if err != nil {
		t.Fatalf("measured zero must not report an error, got %v", err)
	}
	if c != 0 || f != 0 {
		t.Fatalf("no-work delta expected 0/0, got %d/%d", c, f)
	}

	// Add a second commit touching two files.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\nchanged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "add", "a.txt", "b.txt")
	runGitTest(t, dir, "commit", "-q", "-m", "second")

	c, f, err = gitWorkDelta(dir, preHead, preTotal)
	if err != nil {
		t.Fatalf("measured delta must not report an error, got %v", err)
	}
	if c != 1 {
		t.Fatalf("expected 1 commit, got %d", c)
	}
	if f != 2 {
		t.Fatalf("expected 2 files changed, got %d", f)
	}
}

// TestGitWorkDeltaReportsUnmeasured pins the SCHED-GAP-1652 contract: a delta
// that could not be measured must be distinguishable from a measured zero,
// because 27% of the ticks recorded as zero-commit had in fact committed.
func TestGitWorkDeltaReportsUnmeasured(t *testing.T) {
	// Unreadable repo → error, and the zeros must not be read as "no work".
	c, f, err := gitWorkDelta(t.TempDir(), "", 0)
	if err == nil {
		t.Fatalf("non-git dir must report an error so zeros are not read as fact; got %d/%d", c, f)
	}

	// A real repo with NO spawn baseline: the commit count is a floor, not a
	// total, so the caller must still be told the measurement is incomplete.
	dir := initTempRepo(t)
	if _, _, err := gitWorkDelta(dir, "", 0); err == nil {
		t.Fatal("missing spawn baseline must report an incomplete measurement")
	}

	// And a complete baseline yields no error (the control).
	head, total := gitBaseline(dir)
	if _, _, err := gitWorkDelta(dir, head, total); err != nil {
		t.Fatalf("complete baseline should measure cleanly, got %v", err)
	}
}
