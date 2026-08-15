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

	// No work → zero delta.
	c, f := gitWorkDelta(dir, preHead, preTotal)
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

	c, f = gitWorkDelta(dir, preHead, preTotal)
	if c != 1 {
		t.Fatalf("expected 1 commit, got %d", c)
	}
	if f != 2 {
		t.Fatalf("expected 2 files changed, got %d", f)
	}
}
