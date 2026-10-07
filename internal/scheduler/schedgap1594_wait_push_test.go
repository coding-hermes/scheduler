package scheduler

// SCHED-GAP-1594 end-to-end: the push-at-tick-exit gate inside Wait() itself.
// A gateway tick whose workdir gained a commit during the tick window gets
// pushed at Wait() (SCHED-GAP-1694) — unless --disable-tick-push armed
// web-primary, in which case the strand stays local. Reverting the Wait()
// gate makes the DISABLED case push (remote count moves 1 → 2) and this
// test fails: the RED proof for the wiring, not just the flag.

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gap1594CommittedBoardHandler returns a completed gateway response AND
// lands a commit in workdir mid-tick (after Spawn's baseline), so
// countGitChanges measures commits=1 and the Wait() push gate runs.
func gap1594CommittedBoardHandler(t *testing.T, workdir string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// Land the commit during the tick window.
		p := filepath.Join(workdir, "tick-work.txt")
		if err := os.WriteFile(p, []byte("work"), 0o644); err != nil {
			t.Errorf("WriteFile: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for _, args := range [][]string{{"add", "tick-work.txt"}, {"commit", "-m", "tick work"}} {
			cmd := exec.Command("git", append([]string{"-C", workdir}, args...)...)
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
				"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("git %s: %v: %s", strings.Join(args, " "), err, out)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap1594",
			"status": "completed",
			"output": []map[string]any{{
				"type": "message",
				"content": []map[string]any{
					{"type": "output_text", "text": "tick work done"},
				},
			}},
			"usage": map[string]int{},
		})
	}
}

// gap1594StrandWorkdir builds a local repo tracking a bare remote, with one
// pushed baseline commit — the shape a tick's workdir arrives in.
func gap1594StrandWorkdir(t *testing.T) (work, remote string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	remote = filepath.Join(base, "remote.git")
	work = filepath.Join(base, "work")

	gitRun(t, base, "init", "--bare", "-b", "main", remote)
	gitRun(t, base, "init", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "base.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "base.txt")
	gitRun(t, work, "commit", "-m", "baseline")
	gitRun(t, work, "remote", "add", "origin", remote)
	gitRun(t, work, "push", "-u", "origin", "main")
	return work, remote
}

func gap1594RemoteCount(t *testing.T, remote string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", remote, "rev-list", "--count", "main").Output()
	if err != nil {
		t.Fatalf("remote rev-list: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func gap1594RunTick(t *testing.T, workdir string, disablePush bool) {
	t.Helper()
	db := newTestDB(t)
	spawner := schedGap079Spawner(t, db, gap1594CommittedBoardHandler(t, workdir))
	spawner.SetTickPushDisabled(disablePush)

	tick, err := spawner.Spawn(PackedProject{Name: "gap1594", Workdir: workdir}, "gap1594-tick")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	out := tick.Wait()
	if out.Status != TickCompleted {
		t.Fatalf("Wait() status = %s, want %s (error: %s)", out.Status, TickCompleted, out.Error)
	}
	if out.Commits < 1 {
		t.Fatalf("outcome.Commits = %d, want >= 1 — the handler committed mid-window, so the push gate must run", out.Commits)
	}
}

// TestSCHEDGAP1594_DisabledHoldsStrandThroughWait — the actual acceptance:
// with tickPushDisabled armed, a completed tick that landed a commit leaves
// it on local disk (remote count stays at the pushed baseline).
func TestSCHEDGAP1594_DisabledHoldsStrandThroughWait(t *testing.T) {
	work, remote := gap1594StrandWorkdir(t)

	gap1594RunTick(t, work, true)

	if n := gap1594RemoteCount(t, remote); n != "1" {
		t.Errorf("remote main has %s commit(s), want 1 — --disable-tick-push must leave the strand local (the Wait() gate was bypassed or reverted)", n)
	}
}

// TestSCHEDGAP1594_DefaultPushesThroughWait — the zero-value contract holds
// end-to-end: with no flag, the same tick IS pushed at Wait() (remote count
// 1 → 2), SCHED-GAP-1694 behavior unchanged.
func TestSCHEDGAP1594_DefaultPushesThroughWait(t *testing.T) {
	work, remote := gap1594StrandWorkdir(t)

	gap1594RunTick(t, work, false)

	if n := gap1594RemoteCount(t, remote); n != "2" {
		t.Errorf("remote main has %s commit(s), want 2 — the default must push at tick exit (SCHED-GAP-1694)", n)
	}
}
