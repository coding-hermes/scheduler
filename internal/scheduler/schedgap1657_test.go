package scheduler

// SCHED-GAP-1657 — the stale-premise pick gate.
//
// The defect this pins: ~98 zero-commit ticks/week were spent discovering,
// at PICK time, that the board row the foreman was about to work had
// already landed — status still "pending" but commit_hash already present
// (the worker/foreman wrote the commit and forgot to flip the row). The
// gate added here (stalePremiseBlockedQuiet / stalePremiseBlocks,
// stale_premise.go) defers a BUILDER lane whose board holds pending rows
// and EVERY one of them is stale, with the new stale_premise deferral
// reason naming the blocker row(s).
//
// Reporter/parasite lanes keep their timer cadence BY DESIGN (SCHED-GAP-1655
// deliverable 3, laneClass): a stale board never defers them.
//
// Every subtest drives the REAL evaluate() (pinned clock, sim mode) or the
// real gate function — the same harness as admission_decision_test.go and
// schedgap1656_test.go.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSCHEDGAP1657_TasksBuilderStaleRowDefers is AC 1 + AC 2 + AC 3: a
// tasks-mode BUILDER lane whose pending row carries a commit_hash (while
// status is still pending) defers with reason stale_premise, names the row,
// and does NOT dispatch; a pending row with an empty commit_hash is
// unaffected and dispatches normally.
func TestSCHEDGAP1657_TasksBuilderStaleRowDefers(t *testing.T) {
	now := fixedEvalNow()

	t.Run("tasks builder with a stale pending row defers stale_premise and names the row", func(t *testing.T) {
		db := newTestDB(t)
		wd, sha := gitInitWorkdir(t)
		admitWriteBoard(t, wd, fmt.Sprintf(`{"id":"STALE-1","status":"pending","commit_hash":"%s"}`, sha))
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-tasks-stale", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-tasks-stale")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1657-tasks-stale = %d, want exactly 1: %v", len(lines), lines)
		}
		admitAssertLineShape(t, lines[0])
		if got := admitField(lines[0], "reason"); got != AdmissionReasonStalePremise {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonStalePremise, lines[0])
		}
		// The deferral must name the blocker row (the row id is recorded).
		if got := admitField(lines[0], "row"); got != "STALE-1" {
			t.Fatalf("row = %q, want %q (the stale row id must be recorded): %s", got, "STALE-1", lines[0])
		}
		// The deferral means NO session: the sim spawner records every spawn.
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none (deferred, not dispatched)", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonStalePremise] != 1 {
			t.Errorf("admission_counters[stale_premise] = %d, want 1 (%v)", counters[AdmissionReasonStalePremise], counters)
		}
		if n := countDeferralsFor(t, db, "gap1657-tasks-stale", AdmissionReasonStalePremise); n != 1 {
			t.Errorf("deferrals rows with reason stale_premise for gap1657-tasks-stale = %d, want 1", n)
		}
	})

	t.Run("tasks builder with a fresh pending row dispatches normally", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t, `{"id":"FRESH-1","status":"pending","title":"real work"}`)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-tasks-busy", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-tasks-busy")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1657-tasks-busy = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (empty commit_hash → unaffected): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1657-tasks-busy" {
			t.Errorf("simulated selection = %v, want [gap1657-tasks-busy]", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonStalePremise] != 0 {
			t.Errorf("admission_counters[stale_premise] = %d, want 0", counters[AdmissionReasonStalePremise])
		}
	})

	t.Run("tasks builder with one stale and one fresh row dispatches", func(t *testing.T) {
		db := newTestDB(t)
		wd, sha := gitInitWorkdir(t)
		admitWriteBoard(t, wd,
			fmt.Sprintf(`{"id":"STALE-1","status":"pending","commit_hash":"%s"}`, sha),
			`{"id":"FRESH-2","status":"pending"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-tasks-mixed", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-tasks-mixed")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (a fresh pending row is a real candidate): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1657-tasks-mixed" {
			t.Errorf("simulated selection = %v, want [gap1657-tasks-mixed]", got)
		}
	})
}

// TestSCHEDGAP1657_CooldownBuilderStaleRowDefers pins the cooldown-mode half:
// a cooldown BUILDER lane whose pending rows are all stale defers with the
// same reason, and a fresh pending row still proceeds on its pin.
func TestSCHEDGAP1657_CooldownBuilderStaleRowDefers(t *testing.T) {
	now := fixedEvalNow()

	t.Run("cooldown builder with all-stale pending rows defers stale_premise", func(t *testing.T) {
		db := newTestDB(t)
		wd, sha := gitInitWorkdir(t)
		admitWriteBoard(t, wd, fmt.Sprintf(`{"id":"STALE-C","status":"pending","commit_hash":"%s"}`, sha))
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-cooldown-stale", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd, // cooldown mode (default) + builder class
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-cooldown-stale")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonStalePremise {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonStalePremise, lines[0])
		}
		if got := admitField(lines[0], "row"); got != "STALE-C" {
			t.Fatalf("row = %q, want STALE-C: %s", got, lines[0])
		}
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none", got)
		}
	})

	t.Run("cooldown builder with a fresh pending row proceeds on its pin", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t, `{"id":"FRESH-C","status":"pending"}`)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-cooldown-busy", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-cooldown-busy")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1657-cooldown-busy" {
			t.Errorf("simulated selection = %v, want [gap1657-cooldown-busy]", got)
		}
	})
}

// TestSCHEDGAP1657_ReporterExempt is AC 4: a REPORTER lane with the same
// all-stale board keeps its timer cadence (reason ok, spawned), and the
// stale_premise counter stays 0.
func TestSCHEDGAP1657_ReporterExempt(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	wd := admitWorkdirWithBoard(t, `{"id":"STALE-R","status":"pending","commit_hash":"cafebabe"}`)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1657-fleet-sync", CooldownS: 60, Last: now.Add(-90 * time.Second),
		Workdir: wd, // -sync is a REPORTER suffix
	})
	l := admitNewLoop(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("gap1657-fleet-sync")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
		t.Fatalf("reason = %q, want %q (reporter keeps its timer cadence): %s", got, AdmissionReasonOK, lines[0])
	}
	got := simSelectedProjects(t, db)
	if len(got) != 1 || got[0] != "gap1657-fleet-sync" {
		t.Errorf("simulated selection = %v, want [gap1657-fleet-sync]", got)
	}
	if counters := l.AdmissionCounters(); counters[AdmissionReasonStalePremise] != 0 {
		t.Errorf("admission_counters[stale_premise] = %d, want 0 (reporter exempt)", counters[AdmissionReasonStalePremise])
	}
}

// TestSCHEDGAP1657_NamespacePathGate pins the same gate on the namespace
// packer path (packer_select.go) — a tasks-mode builder in a namespace
// defers stale_premise, and is admitted the moment a fresh row exists.
func TestSCHEDGAP1657_NamespacePathGate(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	admitInsertNamespace(t, db, "gap1657ns", 0, "cooldown")
	wd, sha := gitInitWorkdir(t)
	admitWriteBoard(t, wd, fmt.Sprintf(`{"id":"STALE-NS","status":"pending","commit_hash":"%s"}`, sha))
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1657-ns-stale", NS: "gap1657ns", CooldownS: 60, Last: now.Add(-90 * time.Second),
		AdmissionMode: "tasks", Workdir: wd,
	})
	admitInsertCompletedTick(t, db, "tick-ns-stale-1", "gap1657-ns-stale", now.Add(-90*time.Second))
	l := admitNewLoopNS(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("gap1657-ns-stale")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for gap1657-ns-stale = %d, want exactly 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonStalePremise {
		t.Fatalf("reason = %q, want %q (namespace packer path): %s", got, AdmissionReasonStalePremise, lines[0])
	}
	if got := simSelectedProjects(t, db); len(got) != 0 {
		t.Errorf("spawned %v, want none (namespace path deferred the stale builder)", got)
	}
}

// TestSCHEDGAP1657_VocabularyComplete pins AC 6: the new reason is IN the
// frozen vocabulary and is zero-seeded by the counters.
func TestSCHEDGAP1657_VocabularyComplete(t *testing.T) {
	if !admissionReasonIsKnown(AdmissionReasonStalePremise) {
		t.Fatalf("reason %q is not in the frozen vocabulary", AdmissionReasonStalePremise)
	}
	found := false
	for _, r := range admissionReasonVocabulary {
		if r == AdmissionReasonStalePremise {
			found = true
		}
	}
	if !found {
		t.Fatalf("admissionReasonVocabulary missing stale_premise: %v", admissionReasonVocabulary)
	}
	// A boot-fresh loop must zero-seed the counter.
	l := NewLoop(newTestDB(t), 30*time.Second, 24*time.Hour, 10, 100, 4)
	if got := l.AdmissionCounters()[AdmissionReasonStalePremise]; got != 0 {
		t.Errorf("admission_counters[stale_premise] = %d on a boot-fresh loop, want 0", got)
	}
}

// TestSCHEDGAP1657_GateTable pins the gate's conjunction at the function
// level: builder class × mode × ownership × board shape. Every non-blocking
// shape must be transparent.
func TestSCHEDGAP1657_GateTable(t *testing.T) {
	allStale, sha := gitInitWorkdir(t)
	admitWriteBoard(t, allStale, fmt.Sprintf(`{"id":"S-1","status":"pending","commit_hash":"%s"}`, sha))
	withFresh := admitWorkdirWithBoard(t, `{"id":"F-1","status":"pending"}`)
	emptyBoard := admitWorkdirWithBoard(t)
	markdown := t.TempDir()
	writeMarkdownBoard(t, markdown)

	cases := []struct {
		name      string
		project   string
		workdir   string
		mode      string
		ownership string
		wantBlock bool
		wantRow   string
	}{
		{"tasks builder all-stale → blocked, row named", "proj", allStale, "tasks", "", true, "S-1"},
		{"cooldown builder all-stale → blocked, row named", "proj", allStale, "cooldown", "", true, "S-1"},
		{"tasks builder fresh row → proceeds", "proj", withFresh, "tasks", "", false, ""},
		{"cooldown builder fresh row → proceeds", "proj", withFresh, "cooldown", "", false, ""},
		{"tasks builder empty board → proceeds (not this shape)", "proj", emptyBoard, "tasks", "", false, ""},
		{"cooldown builder empty board → proceeds (not this shape)", "proj", emptyBoard, "cooldown", "", false, ""},
		{"reporter all-stale → proceeds (timer by design)", "proj-sync", allStale, "tasks", "", false, ""},
		{"reporter all-stale cooldown → proceeds", "proj-sync", allStale, "cooldown", "", false, ""},
		{"tasks builder foreign board → proceeds (ownership refused)", "proj", allStale, "tasks", "shared", false, ""},
		{"tasks builder no board → proceeds (fail-open)", "proj", t.TempDir(), "tasks", "", false, ""},
		{"tasks builder empty workdir → proceeds (no evidence)", "proj", "", "tasks", "", false, ""},
		{"markdown board → proceeds (no evidence fields)", "proj", markdown, "tasks", "", false, ""},
		{"unknown mode → proceeds (never widen the gate)", "proj", allStale, "bogus", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, rowID := stalePremiseBlockedQuiet(tc.project, tc.workdir, tc.mode, tc.ownership, "")
			if blocked != tc.wantBlock {
				t.Errorf("stalePremiseBlockedQuiet(%q, wd=%q, mode=%q, own=%q) blocked = %v, want %v",
					tc.project, tc.workdir, tc.mode, tc.ownership, blocked, tc.wantBlock)
			}
			if blocked && rowID != tc.wantRow {
				t.Errorf("rowID = %q, want %q", rowID, tc.wantRow)
			}
		})
	}
}

// TestSCHEDGAP1657_FabricatedHashNotStale pins the rework's core fix
// (judge verdict 3de3af0d): a pending row whose commit_hash names a commit
// that never landed — a valid-hex but absent hash — must NOT read as
// stale. Reachability is now git-verified, so a fabricated hash fails the
// probe and the lane DISPATCHES (fail-open) instead of being wrongly
// deferred, which is exactly the false-positive class the verdict called
// out.
func TestSCHEDGAP1657_FabricatedHashNotStale(t *testing.T) {
	now := fixedEvalNow()
	// Valid-hex, 40 chars, but never committed anywhere.
	const fakeHash = "1234567890abcdef1234567890abcdef12345678"

	t.Run("tasks builder with a fake hash dispatches (not stale)", func(t *testing.T) {
		db := newTestDB(t)
		wd, _ := gitInitWorkdir(t) // a REAL repo, so the only missing piece is the hash itself
		admitWriteBoard(t, wd, fmt.Sprintf(`{"id":"FAKE-1","status":"pending","commit_hash":"%s"}`, fakeHash))
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-tasks-fake", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-tasks-fake")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1657-tasks-fake = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (a never-landed hash must not stale-block): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1657-tasks-fake" {
			t.Errorf("simulated selection = %v, want [gap1657-tasks-fake]", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonStalePremise] != 0 {
			t.Errorf("admission_counters[stale_premise] = %d, want 0", counters[AdmissionReasonStalePremise])
		}
	})

	t.Run("cooldown builder with a fake hash proceeds on its pin", func(t *testing.T) {
		db := newTestDB(t)
		wd, _ := gitInitWorkdir(t)
		admitWriteBoard(t, wd, fmt.Sprintf(`{"id":"FAKE-C","status":"pending","commit_hash":"%s"}`, fakeHash))
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-cooldown-fake", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-cooldown-fake")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1657-cooldown-fake" {
			t.Errorf("simulated selection = %v, want [gap1657-cooldown-fake]", got)
		}
	})
}

// TestSCHEDGAP1657_UnresolvableWorkdirFailOpen pins the fail-open carve-out:
// a lane whose workdir cannot resolve to a git repo — a board present but
// no .git (the sim/fixture lane shape) — must never stale-block. The git
// probe fails on a missing repo, so the row reads as NOT stale and the lane
// dispatches. A wrong deferral (real work skipped) is worse than a spent
// tick.
func TestSCHEDGAP1657_UnresolvableWorkdirFailOpen(t *testing.T) {
	now := fixedEvalNow()

	t.Run("board with a hash but no git repo → fail-open, dispatches", func(t *testing.T) {
		db := newTestDB(t)
		// admitWorkdirWithBoard creates a board in a NON-git temp dir —
		// exactly the sim/fixture lane the carve-out protects.
		wd := admitWorkdirWithBoard(t, `{"id":"NOGIT-1","status":"pending","commit_hash":"deadbeef"}`)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1657-nogit", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1657-nogit")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1657-nogit = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (no git repo → fail-open): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1657-nogit" {
			t.Errorf("simulated selection = %v, want [gap1657-nogit]", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonStalePremise] != 0 {
			t.Errorf("admission_counters[stale_premise] = %d, want 0", counters[AdmissionReasonStalePremise])
		}
	})
}

// TestCommitPresentInWorkdir pins the reachability helper directly: a real
// commit hash resolves, a fabricated hash does not, a non-git dir does not,
// an empty workdir/hash does not, and a shell-metacharacter hash is inert
// (passed as an argv element, never interpolated).
func TestCommitPresentInWorkdir(t *testing.T) {
	wd, sha := gitInitWorkdir(t)
	const fakeHash = "1234567890abcdef1234567890abcdef12345678"

	if !commitPresentInWorkdir(wd, sha) {
		t.Errorf("commitPresentInWorkdir(wd, real sha %s) = false, want true", sha)
	}
	if commitPresentInWorkdir(wd, fakeHash) {
		t.Errorf("commitPresentInWorkdir(wd, fake hash) = true, want false")
	}
	if commitPresentInWorkdir(t.TempDir(), sha) {
		t.Errorf("commitPresentInWorkdir(non-git dir, real sha) = true, want false")
	}
	if commitPresentInWorkdir("", sha) {
		t.Errorf("commitPresentInWorkdir(empty workdir) = true, want false")
	}
	if commitPresentInWorkdir(wd, "") {
		t.Errorf("commitPresentInWorkdir(wd, empty hash) = true, want false")
	}
	// Shell-metacharacter hash: must be inert (argv, never sh -c).
	marker := filepath.Join(wd, "pwned")
	inj := "$(touch " + marker + ")"
	if commitPresentInWorkdir(wd, inj) {
		t.Errorf("commitPresentInWorkdir(wd, %q) = true, want false", inj)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("injection marker %s was created — the hash was not passed as a safe argv element", marker)
	}
}

// gitInitWorkdir creates a temp workdir that is a REAL git repo with one
// seed commit (file seed.txt) and returns (workdir, full HEAD sha). Used by
// the SCHED-GAP-1657 rework tests to place a real, resolving commit_hash in
// a board row — the fabricated-hash fixtures of the first attempt cannot
// survive a git-verified gate.
func gitInitWorkdir(t *testing.T) (workdir, sha string) {
	t.Helper()
	wd := t.TempDir()
	runGitTest(t, wd, "init", "-q")
	runGitTest(t, wd, "config", "user.email", "test@example.com")
	runGitTest(t, wd, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wd, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, wd, "add", "seed.txt")
	runGitTest(t, wd, "commit", "-q", "-m", "seed commit for stale-premise")
	sha = strings.TrimSpace(runGitTest(t, wd, "rev-parse", "HEAD"))
	return wd, sha
}

// admitWriteBoard writes JSONL board rows into an existing workdir's
// .coding-hermes/board/tasks.jsonl (the same layout admitWorkdirWithBoard
// creates, but without creating a fresh temp dir — so a real git repo
// workdir can receive its board after its seed commit).
func admitWriteBoard(t *testing.T, workdir string, rows ...string) {
	t.Helper()
	boardDir := filepath.Join(workdir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("mkdir board dir: %v", err)
	}
	body := ""
	if len(rows) > 0 {
		body = strings.Join(rows, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(boardDir, "tasks.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write board file: %v", err)
	}
}

// writeMarkdownBoard creates a legacy markdown board (one unchecked header,
// no evidence fields) so the gate's "markdown boards are never stale"
// branch is exercised.
func writeMarkdownBoard(t *testing.T, workdir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(workdir, ".coding-hermes"), 0o755); err != nil {
		t.Fatalf("mkdir .coding-hermes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".coding-hermes", "tasks.md"),
		[]byte("## [ ] some pending task\n"), 0o644); err != nil {
		t.Fatalf("write tasks.md: %v", err)
	}
}
