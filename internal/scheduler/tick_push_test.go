package scheduler

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// TestSCHEDGAP1694_PushTickWork pins the push-at-tick-exit helper: a tick that
// landed a commit that is still ahead of its upstream gets pushed AND
// verified; a tick that never commits is never pushed (commits==0 short-
// circuits in Wait() before this is called).
func TestSCHEDGAP1694_PushTickWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	work := filepath.Join(base, "work")

	gitRun(t, base, "init", "--bare", "-b", "main", remote)
	gitRun(t, base, "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "a.txt")
	gitRun(t, work, "commit", "-m", "init")
	gitRun(t, work, "remote", "add", "origin", remote)
	gitRun(t, work, "push", "-u", "origin", "main") // sets upstream

	// A fresh, un-pushed commit — exactly the strand the row is about.
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "b.txt")
	gitRun(t, work, "commit", "-m", "second")

	if ok, detail := pushTickWork(work); !ok {
		t.Fatalf("pushTickWork: want ok=true, got false (%s)", detail)
	}
	out, err := exec.Command("git", "-C", remote, "rev-list", "--count", "main").Output()
	if err != nil {
		t.Fatalf("remote rev-list: %v", err)
	}
	if n := strings.TrimSpace(string(out)); n != "2" {
		t.Fatalf("remote main has %s commit(s), want 2 (the strand was not pushed)", n)
	}

	// Idempotent: nothing ahead now → still ok, verified.
	if ok, detail := pushTickWork(work); !ok {
		t.Fatalf("second pushTickWork: want ok=true, got false (%s)", detail)
	}

	// No workdir → honest refusal, never a panic.
	if ok, _ := pushTickWork(""); ok {
		t.Fatal("pushTickWork(\"\") must not report ok")
	}
}
