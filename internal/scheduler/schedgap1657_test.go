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
	"os"
	"path/filepath"
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
		wd := admitWorkdirWithBoard(t, `{"id":"STALE-1","status":"pending","commit_hash":"abc123def"}`)
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
		wd := admitWorkdirWithBoard(t,
			`{"id":"STALE-1","status":"pending","commit_hash":"abc123def"}`,
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
		wd := admitWorkdirWithBoard(t, `{"id":"STALE-C","status":"pending","commit_hash":"deadbeef"}`)
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
	wd := admitWorkdirWithBoard(t, `{"id":"STALE-NS","status":"pending","commit_hash":"abc123"}`)
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
	allStale := admitWorkdirWithBoard(t, `{"id":"S-1","status":"pending","commit_hash":"abc"}`)
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
