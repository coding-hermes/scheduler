package scheduler

// SCHED-GAP-16936 pieces 2 and 3 — the dry-tick RECOVERY PROBE and the CAPPED
// repair re-dispatch that consumes it.
//
// Piece 2's rule: a concluded dry (or failed) tick asks git ONE question about
// its own worktree — is there uncommitted CONTENT a later tick could harvest —
// and books the answer on the tick record as a closed-vocabulary verdict
// (no_changes / recovery_available / not_probed), with foreman bookkeeping
// (.coding-hermes/) counted separately and never deciding it.
//
// Piece 3's rule: only a FAILED tick whose probe found content is
// repair-eligible; at most TWO repair re-dispatches per failure chain; the
// attempt this row caused and the cause (evidence or refusal) ride the same
// finalization UPDATE. Nothing runnable ⇒ no re-dispatch, and the row still
// records why it was not retried.

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sched16936GitRepo builds a real git worktree holding ONE commit and returns
// its path. The probe is a git-reading function, so its tests use git rather
// than a fake: a hand-written porcelain fixture can only prove the parser (below)
// and would happily pass while the invocation itself was wrong.
func sched16936GitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sched16936Git(t, dir, "init", "-q", "-b", "main")
	writeLines(t, filepath.Join(dir, "seed.txt"), "seed")
	sched16936Git(t, dir, "add", "-A")
	sched16936Git(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

// sched16936Git runs one git command and fails the test on a non-zero exit.
// Identity and hooks are pinned per-invocation so a host-level git config
// cannot make the fixture commit fail (or run somebody's pre-commit hook).
func sched16936Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.email=worker@example.invalid",
		"-c", "user.name=worker",
		"-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null",
		"-C", dir,
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// sched16936Project creates the lane row the probe resolves its workdir from.
// projects.repo_url is NOT NULL, so the fixture supplies the URL the schema
// demands rather than relying on a default the table does not have.
func sched16936Project(t *testing.T, db *sql.DB, name, workdir string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO projects (name, repo_url, workdir, enabled, cooldown_s, created_at, updated_at)
		 VALUES (?, ?, ?, 1, 900, datetime('now'), datetime('now'))`,
		name, "https://github.com/example/"+name, workdir,
	); err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
}

// sched16936Tick reads back the four columns this row owns.
func sched16936Tick(t *testing.T, db *sql.DB, tickID string) (state, detail, errText, repairCause string, repairAttempt int) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT recovery_state, recovery_detail, COALESCE(error, ''), repair_cause, repair_attempt
		 FROM ticks WHERE id = ?`, tickID,
	).Scan(&state, &detail, &errText, &repairCause, &repairAttempt); err != nil {
		t.Fatalf("read tick %s: %v", tickID, err)
	}
	return state, detail, errText, repairCause, repairAttempt
}

// --- piece 2: the classifier (pure) ------------------------------------------

// TestSCHEDGAP16936_ClassifyPorcelainStatus pins the verdict derivation itself:
// the porcelain shapes that mean "content", the bookkeeping exclusion, the
// bounded sample and the sorted order.
func TestSCHEDGAP16936_ClassifyPorcelainStatus(t *testing.T) {
	cases := []struct {
		name          string
		out           string
		wantState     string
		wantModified  int
		wantUntracked int
		wantBookkeep  int
		wantSamples   []string
	}{
		{
			name:      "clean tree is no_changes",
			out:       "",
			wantState: RecoveryStateNoChanges,
		},
		{
			// A branch header is not a status entry (porcelain -b shape).
			name:      "branch header ignored",
			out:       "## main...origin/main\n",
			wantState: RecoveryStateNoChanges,
		},
		{
			name:         "modified tracked path",
			out:          " M internal/foo/a.go\n",
			wantState:    RecoveryStateAvailable,
			wantModified: 1,
			wantSamples:  []string{"internal/foo/a.go"},
		},
		{
			name:         "staged addition counts as modified",
			out:          "A  internal/foo/b.go\n",
			wantState:    RecoveryStateAvailable,
			wantModified: 1,
			wantSamples:  []string{"internal/foo/b.go"},
		},
		{
			name:          "untracked content counts",
			out:           "?? notes.md\n",
			wantState:     RecoveryStateAvailable,
			wantUntracked: 1,
			wantSamples:   []string{"notes.md"},
		},
		{
			// The DESTINATION is the path that exists now.
			name:         "rename samples the destination",
			out:          "R  old/a.go -> new/a.go\n",
			wantState:    RecoveryStateAvailable,
			wantModified: 1,
			wantSamples:  []string{"new/a.go"},
		},
		{
			// The whole point of the exclusion: a wave tick ALWAYS writes
			// bookkeeping, so counting it would make every wave tick
			// recovery_available and the verdict useless.
			name:         "bookkeeping alone is NOT content",
			out:          "?? .coding-hermes/board/tasks.jsonl\n M .coding-hermes/waves/t1.json\n",
			wantState:    RecoveryStateNoChanges,
			wantBookkeep: 2,
		},
		{
			name:          "bookkeeping counted next to content",
			out:           "?? .coding-hermes/board/tasks.jsonl\n M internal/foo/a.go\n?? scratch.sh\n",
			wantState:     RecoveryStateAvailable,
			wantModified:  1,
			wantUntracked: 1,
			wantBookkeep:  1,
			wantSamples:   []string{"internal/foo/a.go", "scratch.sh"},
		},
		{
			// git emits status entries path-sorted, so the bound keeps the
			// alphabetically first three — the field stays a bounded sample of
			// the FIRST paths, never an arbitrary window.
			name:         "sample is bounded to the first three paths",
			out:          " M v.go\n M w.go\n M x.go\n M y.go\n M z.go\n",
			wantState:    RecoveryStateAvailable,
			wantModified: 5,
			wantSamples:  []string{"v.go", "w.go", "x.go"},
		},
		{
			// Hand-fed (out-of-order) input is still reported deterministically.
			name:          "sample is sorted",
			out:           " M z.go\n?? a.go\n M m.go\n",
			wantState:     RecoveryStateAvailable,
			wantModified:  2,
			wantUntracked: 1,
			wantSamples:   []string{"a.go", "m.go", "z.go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyPorcelainStatus(tc.out)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q (out=%q)", got.State, tc.wantState, tc.out)
			}
			if got.Modified != tc.wantModified {
				t.Errorf("modified = %d, want %d", got.Modified, tc.wantModified)
			}
			if got.Untracked != tc.wantUntracked {
				t.Errorf("untracked = %d, want %d", got.Untracked, tc.wantUntracked)
			}
			if got.Bookkeeping != tc.wantBookkeep {
				t.Errorf("bookkeeping = %d, want %d", got.Bookkeeping, tc.wantBookkeep)
			}
			if strings.Join(got.Samples, ",") != strings.Join(tc.wantSamples, ",") {
				t.Errorf("samples = %v, want %v", got.Samples, tc.wantSamples)
			}
			// The verdict and the counters must agree — a state that claims
			// available with zero content (or the reverse) is unreadable.
			if avail := got.State == RecoveryStateAvailable; avail != (tc.wantModified+tc.wantUntracked > 0) {
				t.Errorf("state %q disagrees with its counters (modified=%d untracked=%d)", got.State, got.Modified, got.Untracked)
			}
		})
	}
}

// TestSCHEDGAP16936_RecoveryVocabularyIsClosed pins the Go twin of the v92
// CHECK: exactly the three members, and "" is NOT one of them (it is the honest
// "no verdict owed", not a fourth state).
func TestSCHEDGAP16936_RecoveryVocabularyIsClosed(t *testing.T) {
	for _, v := range []string{RecoveryStateNoChanges, RecoveryStateAvailable, RecoveryStateNotProbed} {
		if !RecoveryStateIsValid(v) {
			t.Errorf("RecoveryStateIsValid(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "clean", "dirty", "NO_CHANGES", "recovery"} {
		if RecoveryStateIsValid(v) {
			t.Errorf("RecoveryStateIsValid(%q) = true — outside the closed vocabulary", v)
		}
	}
	if len(RecoveryStates) != 3 {
		t.Errorf("RecoveryStates has %d members, want 3", len(RecoveryStates))
	}
}

// --- piece 2: the probe against real worktrees -------------------------------

// TestSCHEDGAP16936_ProbeWorktreeRecovery walks the three verdicts a real
// worktree can produce, and the one it can NEVER produce: an unmeasurable tree
// reported as clean.
func TestSCHEDGAP16936_ProbeWorktreeRecovery(t *testing.T) {
	ctx := context.Background()

	t.Run("clean", func(t *testing.T) {
		repo := sched16936GitRepo(t)
		got := probeWorktreeRecovery(ctx, repo)
		if got.State != RecoveryStateNoChanges {
			t.Fatalf("state = %q, want %q (%s)", got.State, RecoveryStateNoChanges, got.Detail)
		}
		if got.Recoverable() {
			t.Error("Recoverable() = true on a clean worktree")
		}
		if got.Modified != 0 || got.Untracked != 0 {
			t.Errorf("counts = (%d,%d), want (0,0)", got.Modified, got.Untracked)
		}
	})

	t.Run("dirty tracked file", func(t *testing.T) {
		repo := sched16936GitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("changed"), 0o644); err != nil {
			t.Fatalf("dirty the fixture: %v", err)
		}
		got := probeWorktreeRecovery(ctx, repo)
		if got.State != RecoveryStateAvailable {
			t.Fatalf("state = %q, want %q (%s)", got.State, RecoveryStateAvailable, got.Detail)
		}
		if !got.Recoverable() {
			t.Error("Recoverable() = false on a dirty worktree")
		}
		if got.Modified == 0 {
			t.Error("modified = 0 on a modified tracked file")
		}
		if !strings.Contains(got.Detail, "seed.txt") {
			t.Errorf("detail %q does not name the changed path", got.Detail)
		}
	})

	t.Run("untracked content", func(t *testing.T) {
		repo := sched16936GitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "leftover.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("write untracked fixture: %v", err)
		}
		got := probeWorktreeRecovery(ctx, repo)
		if got.State != RecoveryStateAvailable || got.Untracked == 0 {
			t.Fatalf("state/counts = (%q, untracked=%d), want (%q, >0)", got.State, got.Untracked, RecoveryStateAvailable)
		}
	})

	t.Run("bookkeeping only is clean", func(t *testing.T) {
		repo := sched16936GitRepo(t)
		if err := os.MkdirAll(filepath.Join(repo, ".coding-hermes", "board"), 0o755); err != nil {
			t.Fatalf("mkdir bookkeeping: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".coding-hermes", "board", "tasks.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write bookkeeping: %v", err)
		}
		got := probeWorktreeRecovery(ctx, repo)
		if got.State != RecoveryStateNoChanges {
			t.Fatalf("state = %q, want %q — a wave tick's own paperwork is not harvestable content (%s)",
				got.State, RecoveryStateNoChanges, got.Detail)
		}
		if got.Bookkeeping == 0 {
			t.Error("bookkeeping = 0 — the excluded paths must still be counted for honesty")
		}
	})
}

// TestSCHEDGAP16936_ProbeUnmeasurableIsNotClean is the third state's reason to
// exist: every way the probe can fail must read not_probed, never no_changes
// ("could not measure" is not "clean" — clean is a positive claim somebody acts
// on).
func TestSCHEDGAP16936_ProbeUnmeasurableIsNotClean(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		workdir string
		want    string // substring of Detail
	}{
		{name: "no workdir", workdir: "", want: "no workdir"},
		{name: "missing directory", workdir: filepath.Join(t.TempDir(), "does-not-exist"), want: "not a directory"},
		{name: "not a git worktree", workdir: t.TempDir(), want: "not a git worktree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := probeWorktreeRecovery(ctx, tc.workdir)
			if got.State != RecoveryStateNotProbed {
				t.Fatalf("state = %q, want %q (detail %q)", got.State, RecoveryStateNotProbed, got.Detail)
			}
			if got.Recoverable() {
				t.Error("Recoverable() = true on an unmeasured worktree")
			}
			if !strings.Contains(got.Detail, tc.want) {
				t.Errorf("detail = %q, want it to name %q", got.Detail, tc.want)
			}
		})
	}
}

// TestSCHEDGAP16936_DryTickBooksRecoveryProbe is the acceptance's row-level
// claim: a dry tick's RECORD distinguishes a clean tree from one holding
// content. Before this row both rows read identically (dry_run + no_changes).
func TestSCHEDGAP16936_DryTickBooksRecoveryProbe(t *testing.T) {
	now := time.Now().UTC()

	t.Run("clean worktree records no_changes", func(t *testing.T) {
		db := newTestDB(t)
		repo := sched16936GitRepo(t)
		sched16936Project(t, db, "lane-clean", repo)
		lt := NewLifecycleTracker(db)
		tickID := "lane-clean-1"
		if err := lt.Enqueue("lane-clean", tickID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := lt.Complete(TickOutcome{
			TickID: tickID, Project: "lane-clean",
			Started: now.Add(-time.Minute), Finished: now,
			Status: TickCompleted, ExitCode: 0, SessionID: "sess-clean",
		}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		state, detail, errText, _, _ := sched16936Tick(t, db, tickID)
		if state != RecoveryStateNoChanges {
			t.Fatalf("recovery_state = %q, want %q (%s)", state, RecoveryStateNoChanges, detail)
		}
		if !strings.Contains(detail, "modified=0") || !strings.Contains(detail, "untracked=0") {
			t.Errorf("recovery_detail = %q, want the exact counts", detail)
		}
		if errText != "no_changes" {
			t.Errorf("dry sentinel = %q, want no_changes", errText)
		}
	})

	t.Run("worktree holding content records recovery_available", func(t *testing.T) {
		db := newTestDB(t)
		repo := sched16936GitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("uncommitted work"), 0o644); err != nil {
			t.Fatalf("dirty the fixture: %v", err)
		}
		sched16936Project(t, db, "lane-dirty", repo)
		lt := NewLifecycleTracker(db)
		tickID := "lane-dirty-1"
		if err := lt.Enqueue("lane-dirty", tickID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := lt.Complete(TickOutcome{
			TickID: tickID, Project: "lane-dirty",
			Started: now.Add(-time.Minute), Finished: now,
			Status: TickCompleted, ExitCode: 0, SessionID: "sess-dirty",
		}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		state, detail, errText, _, _ := sched16936Tick(t, db, tickID)
		if state != RecoveryStateAvailable {
			t.Fatalf("recovery_state = %q, want %q (%s)", state, RecoveryStateAvailable, detail)
		}
		if !strings.Contains(detail, "seed.txt") {
			t.Errorf("recovery_detail = %q, want the changed path named", detail)
		}
		if errText != "recovery_available" {
			t.Errorf("dry sentinel = %q, want recovery_available (no_changes would be a false claim about the tree)", errText)
		}
	})

	t.Run("productive tick owes no verdict", func(t *testing.T) {
		db := newTestDB(t)
		repo := sched16936GitRepo(t)
		if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("irrelevant"), 0o644); err != nil {
			t.Fatalf("dirty the fixture: %v", err)
		}
		sched16936Project(t, db, "lane-productive", repo)
		lt := NewLifecycleTracker(db)
		tickID := "lane-productive-1"
		if err := lt.Enqueue("lane-productive", tickID); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := lt.Complete(TickOutcome{
			TickID: tickID, Project: "lane-productive",
			Started: now.Add(-time.Minute), Finished: now,
			Status: TickCompleted, ExitCode: 0, SessionID: "sess-productive",
			Commits: 1, FilesChanged: 3,
		}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		state, _, errText, cause, attempt := sched16936Tick(t, db, tickID)
		if state != "" {
			t.Errorf("recovery_state = %q on a productive tick, want the honest empty", state)
		}
		if cause != "" || attempt != 0 {
			t.Errorf("repair ledger = (%q, %d) on a completed tick, want empty", cause, attempt)
		}
		if errText == "recovery_available" || errText == "no_changes" {
			t.Errorf("productive tick carried the dry sentinel %q", errText)
		}
	})
}

// TestSCHEDGAP16936_ReasonCodeRefusesNoWorkOnDirtyTree pins the vocabulary
// integration: a MEASURED no-work transcript on a worktree that holds content
// must not be coded no_work ("the lane had nothing to do" is refuted by the
// tree sitting right there).
func TestSCHEDGAP16936_ReasonCodeRefusesNoWorkOnDirtyTree(t *testing.T) {
	base := TickOutcome{
		TickID: "t1", Project: "lane", Status: TickCompleted,
		TokensIn: 100, TokensOut: 20, Commits: 0, FilesChanged: 0,
	}
	clean := base
	clean.RecoveryState = RecoveryStateNoChanges
	if got := resolveTickReasonCode(clean, "no_work", "", 0); got != "no_work" {
		t.Errorf("clean tree + measured no-work transcript = %q, want no_work", got)
	}
	dirty := base
	dirty.RecoveryState = RecoveryStateAvailable
	if got := resolveTickReasonCode(dirty, "no_work", "", 0); got == "no_work" {
		t.Errorf("dirty worktree was still coded no_work — the probe verdict must refute it")
	}
}

// --- piece 3: the decision table (pure) --------------------------------------

// TestSCHEDGAP16936_DecideRepairRedispatch pins eligibility + the cap as a
// table: only a FAILED tick with content is re-dispatchable, attempts 1 and 2
// dispatch, attempt 3 is refused, nothing runnable never dispatches.
func TestSCHEDGAP16936_DecideRepairRedispatch(t *testing.T) {
	dirty := RecoveryProbe{State: RecoveryStateAvailable, Detail: "modified=1 untracked=0"}
	clean := RecoveryProbe{State: RecoveryStateNoChanges, Detail: "modified=0 untracked=0"}
	unprobed := RecoveryProbe{State: RecoveryStateNotProbed, Detail: "not a git worktree"}

	cases := []struct {
		name         string
		status       TickStatus
		probe        RecoveryProbe
		prior        int
		wantDispatch bool
		wantRefused  bool
		wantAttempt  int
		wantCauseHas string
	}{
		{name: "completed is never repaired", status: TickCompleted, probe: dirty, wantCauseHas: ""},
		{name: "timeout is never repaired", status: TickTimeout, probe: dirty, wantCauseHas: ""},
		{name: "deferred is never repaired", status: TickDeferred, probe: dirty, wantCauseHas: ""},
		{
			name: "failed with content dispatches attempt 1", status: TickFailed, probe: dirty, prior: 0,
			wantDispatch: true, wantAttempt: 1, wantCauseHas: RepairCauseRecoverable,
		},
		{
			name: "second attempt dispatches", status: TickFailed, probe: dirty, prior: 1,
			wantDispatch: true, wantAttempt: 2, wantCauseHas: RepairCauseRecoverable,
		},
		{
			name: "third attempt is refused", status: TickFailed, probe: dirty, prior: 2,
			wantRefused: true, wantAttempt: 3, wantCauseHas: RepairCauseCapReached,
		},
		{
			name: "beyond the cap stays refused", status: TickFailed, probe: dirty, prior: 9,
			wantRefused: true, wantAttempt: 10, wantCauseHas: RepairCauseCapReached,
		},
		{
			name: "clean worktree is not repaired", status: TickFailed, probe: clean,
			wantCauseHas: RepairCauseNothingRunnable,
		},
		{
			// An unmeasured tree is not evidence of leftovers: fail closed.
			name: "unmeasured worktree is not repaired", status: TickFailed, probe: unprobed,
			wantCauseHas: RepairCauseNothingRunnable,
		},
		{
			// A negative count must clamp, not grant extra attempts.
			name: "negative prior clamps to zero", status: TickFailed, probe: dirty, prior: -3,
			wantDispatch: true, wantAttempt: 1, wantCauseHas: RepairCauseRecoverable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideRepairRedispatch(tc.status, tc.probe, tc.prior)
			if got.Dispatch != tc.wantDispatch {
				t.Errorf("Dispatch = %v, want %v (%+v)", got.Dispatch, tc.wantDispatch, got)
			}
			if got.Refused != tc.wantRefused {
				t.Errorf("Refused = %v, want %v (%+v)", got.Refused, tc.wantRefused, got)
			}
			if got.Dispatch && got.Attempt != tc.wantAttempt {
				t.Errorf("Attempt = %d, want %d", got.Attempt, tc.wantAttempt)
			}
			if tc.wantCauseHas == "" {
				if got.Cause != "" {
					t.Errorf("Cause = %q, want empty for an ineligible tick", got.Cause)
				}
				return
			}
			if !strings.HasPrefix(got.Cause, tc.wantCauseHas) {
				t.Errorf("Cause = %q, want prefix %q", got.Cause, tc.wantCauseHas)
			}
		})
	}
}

// --- piece 3: the chain walk (DB) --------------------------------------------

// TestSCHEDGAP16936_RepairChainPerFailure walks priorRepairAttempts: failed and
// timed-out rows continue the chain, anything else ends it (so "at most twice"
// is per FAILURE, not per lane lifetime), and ordering is by insertion.
func TestSCHEDGAP16936_RepairChainPerFailure(t *testing.T) {
	db := newTestDB(t)
	sched16936Project(t, db, "lane", t.TempDir())
	lt := NewLifecycleTracker(db)
	ctx := context.Background()

	enqueue := func(id string) {
		t.Helper()
		if err := lt.Enqueue("lane", id); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}
	setRow := func(id, status string, attempt int) {
		t.Helper()
		if _, err := db.Exec(`UPDATE ticks SET status = ?, repair_attempt = ? WHERE id = ?`, status, attempt, id); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}

	// A productively completed tick first: it must NOT be walked past, so the
	// first failure after it starts a fresh chain at 0.
	if got := priorRepairAttempts(ctx, db, "lane", "missing-tick"); got != 0 {
		t.Fatalf("priorRepairAttempts for an unknown tick = %d, want 0", got)
	}

	enqueue("t-done")
	setRow("t-done", string(TickCompleted), 0)
	enqueue("t-fail-1")
	if got := priorRepairAttempts(ctx, db, "lane", "t-fail-1"); got != 0 {
		t.Errorf("prior attempts after a completed tick = %d, want 0 (a completion ends the chain)", got)
	}
	setRow("t-fail-1", string(TickFailed), 1)
	enqueue("t-fail-2")
	if got := priorRepairAttempts(ctx, db, "lane", "t-fail-2"); got != 1 {
		t.Errorf("prior attempts inside the chain = %d, want 1", got)
	}
	setRow("t-fail-2", string(TickFailed), 2)
	enqueue("t-timeout")
	setRow("t-timeout", string(TickTimeout), 0) // a timeout neither adds an attempt...
	enqueue("t-fail-3")
	if got := priorRepairAttempts(ctx, db, "lane", "t-fail-3"); got != 2 {
		// ...nor breaks the chain: the max attempt seen in the tail is 2.
		t.Errorf("prior attempts across a timeout = %d, want 2", got)
	}

	// Another lane's rows never leak into this chain.
	sched16936Project(t, db, "other", t.TempDir())
	if err := lt.Enqueue("other", "other-fail"); err != nil {
		t.Fatalf("Enqueue(other): %v", err)
	}
	setRow("other-fail", string(TickFailed), 2)
	if got := priorRepairAttempts(ctx, db, "other", "other-fail"); got != 0 {
		t.Errorf("cross-lane walk = %d, want 0 (chains are per project)", got)
	}
}

// --- piece 3: the capped re-dispatch on the tick record ----------------------

// TestSCHEDGAP16936_RepairRedispatchCappedAtTwo is the acceptance's central
// claim at the record level: a failed tick with content books attempt 1, the
// next books 2, the third is REFUSED with the cause recorded — and a failure
// with nothing runnable never books an attempt at all.
func TestSCHEDGAP16936_RepairRedispatchCappedAtTwo(t *testing.T) {
	now := time.Now().UTC()
	db := newTestDB(t)
	repo := sched16936GitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("leftover work"), 0o644); err != nil {
		t.Fatalf("dirty the fixture: %v", err)
	}
	sched16936Project(t, db, "lane", repo)
	lt := NewLifecycleTracker(db)

	failTick := func(id string) RepairRedispatchDecision {
		t.Helper()
		if err := lt.Enqueue("lane", id); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
		decision, err := lt.CompleteWithRepair(TickOutcome{
			TickID: id, Project: "lane",
			Started: now.Add(-2 * time.Minute), Finished: now,
			Status: TickFailed, ExitCode: 1, SessionID: "sess-" + id,
			Error: "worker exited 1",
		})
		if err != nil {
			t.Fatalf("CompleteWithRepair(%s): %v", id, err)
		}
		return decision
	}

	first := failTick("lane-fail-1")
	if !first.Dispatch || first.Attempt != 1 {
		t.Fatalf("first failure decision = %+v, want dispatch attempt 1", first)
	}
	state, _, _, cause, attempt := sched16936Tick(t, db, "lane-fail-1")
	if state != RecoveryStateAvailable {
		t.Errorf("recovery_state = %q, want %q", state, RecoveryStateAvailable)
	}
	if attempt != 1 {
		t.Errorf("repair_attempt = %d, want 1", attempt)
	}
	if !strings.HasPrefix(cause, RepairCauseRecoverable) {
		t.Errorf("repair_cause = %q, want the recoverable evidence", cause)
	}

	second := failTick("lane-fail-2")
	if !second.Dispatch || second.Attempt != 2 {
		t.Fatalf("second failure decision = %+v, want dispatch attempt 2", second)
	}
	if _, _, _, _, attempt := sched16936Tick(t, db, "lane-fail-2"); attempt != 2 {
		t.Errorf("second repair_attempt = %d, want 2", attempt)
	}

	third := failTick("lane-fail-3")
	if third.Dispatch {
		t.Fatalf("third failure DISPATCHED (%+v) — the cap is 2", third)
	}
	if !third.Refused {
		t.Errorf("third failure decision = %+v, want Refused", third)
	}
	_, _, _, cause, attempt = sched16936Tick(t, db, "lane-fail-3")
	if attempt != 0 {
		t.Errorf("refused row repair_attempt = %d, want 0 (it caused no attempt)", attempt)
	}
	if !strings.HasPrefix(cause, RepairCauseCapReached) {
		t.Errorf("refused row repair_cause = %q, want %q", cause, RepairCauseCapReached)
	}

	// The chain restarts after a completion: the cap is per failure, not per
	// lane lifetime.
	if err := lt.Enqueue("lane", "lane-ok"); err != nil {
		t.Fatalf("Enqueue(lane-ok): %v", err)
	}
	if err := lt.Complete(TickOutcome{
		TickID: "lane-ok", Project: "lane",
		Started: now.Add(-time.Minute), Finished: now,
		Status: TickCompleted, ExitCode: 0, SessionID: "sess-ok", Commits: 1,
	}); err != nil {
		t.Fatalf("Complete(lane-ok): %v", err)
	}
	fourth := failTick("lane-fail-4")
	if !fourth.Dispatch || fourth.Attempt != 1 {
		t.Fatalf("failure after a completed tick = %+v, want a fresh chain at attempt 1", fourth)
	}
}

// TestSCHEDGAP16936_RepairNothingRunnableIsNotRetried pins the other half: a
// failure on a CLEAN worktree books no attempt and records why.
func TestSCHEDGAP16936_RepairNothingRunnableIsNotRetried(t *testing.T) {
	now := time.Now().UTC()
	db := newTestDB(t)
	repo := sched16936GitRepo(t) // clean: nothing to repair
	sched16936Project(t, db, "lane", repo)
	lt := NewLifecycleTracker(db)

	if err := lt.Enqueue("lane", "lane-fail-clean"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	decision, err := lt.CompleteWithRepair(TickOutcome{
		TickID: "lane-fail-clean", Project: "lane",
		Started: now.Add(-2 * time.Minute), Finished: now,
		Status: TickFailed, ExitCode: 1, SessionID: "sess-clean", Error: "worker exited 1",
	})
	if err != nil {
		t.Fatalf("CompleteWithRepair: %v", err)
	}
	if decision.Dispatch || decision.Refused {
		t.Fatalf("clean-worktree failure decision = %+v, want neither dispatch nor a cap refusal", decision)
	}
	state, _, _, cause, attempt := sched16936Tick(t, db, "lane-fail-clean")
	if state != RecoveryStateNoChanges {
		t.Errorf("recovery_state = %q, want %q", state, RecoveryStateNoChanges)
	}
	if attempt != 0 {
		t.Errorf("repair_attempt = %d, want 0", attempt)
	}
	if !strings.HasPrefix(cause, RepairCauseNothingRunnable) {
		t.Errorf("repair_cause = %q, want %q", cause, RepairCauseNothingRunnable)
	}
}

// TestSCHEDGAP16936_RepairKillSwitchRecordsButDoesNotBook proves the disarm path
// is visible AND does not spend the chain's attempts: nothing is booked
// (repair_attempt stays 0 because nothing ran), and the cause says so.
func TestSCHEDGAP16936_RepairKillSwitchRecordsButDoesNotBook(t *testing.T) {
	t.Setenv("SCHEDULER_REPAIR_REDISPATCH", "0")
	if repairRedispatchArmed() {
		t.Fatal("repairRedispatchArmed() = true with SCHEDULER_REPAIR_REDISPATCH=0")
	}
	now := time.Now().UTC()
	db := newTestDB(t)
	repo := sched16936GitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("leftover work"), 0o644); err != nil {
		t.Fatalf("dirty the fixture: %v", err)
	}
	sched16936Project(t, db, "lane", repo)
	lt := NewLifecycleTracker(db)

	if err := lt.Enqueue("lane", "lane-fail-disarmed"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	decision, err := lt.CompleteWithRepair(TickOutcome{
		TickID: "lane-fail-disarmed", Project: "lane",
		Started: now.Add(-2 * time.Minute), Finished: now,
		Status: TickFailed, ExitCode: 1, SessionID: "sess-disarmed", Error: "worker exited 1",
	})
	if err != nil {
		t.Fatalf("CompleteWithRepair: %v", err)
	}
	if decision.Dispatch {
		t.Fatalf("decision = %+v, want no dispatch while disarmed", decision)
	}
	_, _, _, cause, attempt := sched16936Tick(t, db, "lane-fail-disarmed")
	if attempt != 0 {
		t.Errorf("repair_attempt = %d while disarmed, want 0 (an attempt is an action that ran)", attempt)
	}
	if !strings.Contains(cause, "not launched") {
		t.Errorf("repair_cause = %q, want the not-launched marker", cause)
	}
	if !strings.Contains(cause, RecoveryStateAvailable) {
		t.Errorf("repair_cause = %q, want the recovery evidence preserved", cause)
	}

	// And a disarmed decision MUST NOT look like the cap: the chain is intact.
	t.Setenv("SCHEDULER_REPAIR_REDISPATCH", "1")
	if !repairRedispatchArmed() {
		t.Fatal("repairRedispatchArmed() = false for SCHEDULER_REPAIR_REDISPATCH=1")
	}
	if err := lt.Enqueue("lane", "lane-fail-armed"); err != nil {
		t.Fatalf("Enqueue(armed): %v", err)
	}
	armed, err := lt.CompleteWithRepair(TickOutcome{
		TickID: "lane-fail-armed", Project: "lane",
		Started: now.Add(-2 * time.Minute), Finished: now,
		Status: TickFailed, ExitCode: 1, SessionID: "sess-armed", Error: "worker exited 1",
	})
	if err != nil {
		t.Fatalf("CompleteWithRepair(armed): %v", err)
	}
	if !armed.Dispatch || armed.Attempt != 1 {
		t.Fatalf("re-armed decision = %+v, want attempt 1 (the disarm must not have spent an attempt)", armed)
	}
}

// TestSCHEDGAP16936_CompleteTickReturnsBookedRepair pins the slot pool seam: the
// finalizer's verdict is what the pool launches, and a disarmed fleet launches
// nothing.
func TestSCHEDGAP16936_CompleteTickReturnsBookedRepair(t *testing.T) {
	now := time.Now().UTC()
	db := newTestDB(t)
	repo := sched16936GitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("leftover work"), 0o644); err != nil {
		t.Fatalf("dirty the fixture: %v", err)
	}
	sched16936Project(t, db, "lane", repo)
	lt := NewLifecycleTracker(db)
	pool := NewSlotPool(4, nil, lt)
	proj := PackedProject{Name: "lane"}

	run := func(id string) *repairRequest {
		t.Helper()
		if err := lt.Enqueue("lane", id); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
		return pool.completeTick(proj, TickOutcome{
			TickID: id, Project: "lane",
			Started: now.Add(-2 * time.Minute), Finished: now,
			Status: TickFailed, ExitCode: 1, SessionID: "sess-" + id, Error: "worker exited 1",
		}, false)
	}

	req := run("lane-pool-1")
	if req == nil {
		t.Fatal("completeTick returned no repair request for a recoverable failure")
	}
	if req.attempt != 1 || req.proj.Name != "lane" {
		t.Errorf("repair request = %+v, want attempt 1 for lane", req)
	}

	t.Setenv("SCHEDULER_REPAIR_REDISPATCH", "off")
	if req := run("lane-pool-2"); req != nil {
		t.Fatalf("completeTick returned %+v while disarmed, want nil", req)
	}
}
