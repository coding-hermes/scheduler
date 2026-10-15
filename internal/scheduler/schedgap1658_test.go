package scheduler

// SCHED-GAP-1658 — the blocked-head pick gate (dependency_blocked and
// sibling_held).
//
// The defect this pins: a tick can be spent discovering, at PICK time,
// that it cannot proceed — the board shows pending rows but every one of
// them is a head the foreman cannot take:
//   - DEPENDENCY-BLOCKED: the row's depends_on names a row that is not
//     complete (the wave planner skips it);
//   - SIBLING-HELD: the row is explicitly claimed/in_progress by a
//     sibling lane (worker_status field).
//
// The gate added here (premiseBlockedQuiet / premiseBlocks /
// premiseBoardShapeOf, premise_blocked.go) defers a BUILDER lane whose
// board holds pending rows and EVERY one of them is blocked or held, with
// the new dependency_blocked / sibling_held deferral reasons naming the
// untakeable head(s).
//
// Composition law: the SCHED-GAP-1657 stale-premise gate keeps priority —
// every call site consults stalePremise* first, so a stale+blocked board
// reports stale_premise and is never double-deferred.
//
// Every evaluate-level subtest drives the REAL evaluate() (pinned clock,
// sim mode) or the real gate function — the same harness as
// schedgap1657_test.go and admission_decision_test.go.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestSCHEDGAP1658_DependencyBlockedDefers is AC 1 + AC 3 + AC 4: a
// BUILDER lane whose pending rows ALL depend on a non-complete row defers
// with reason dependency_blocked and names the blocked rows; one free
// pending row defeats the gate; a depends_on naming a COMPLETE row is
// free and dispatches normally (row order on the board must not matter).
func TestSCHEDGAP1658_DependencyBlockedDefers(t *testing.T) {
	now := fixedEvalNow()

	t.Run("tasks builder with all pending rows dependency-blocked defers and names the rows", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t,
			`{"id":"DEP-A","status":"pending","depends_on":["BLOCKER-1"]}`,
			`{"id":"DEP-B","status":"pending","depends_on":["BLOCKER-1"]}`,
			`{"id":"BLOCKER-1","status":"in_progress"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-tasks-dep", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-tasks-dep")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1658-tasks-dep = %d, want exactly 1: %v", len(lines), lines)
		}
		admitAssertLineShape(t, lines[0])
		if got := admitField(lines[0], "reason"); got != AdmissionReasonDependencyBlocked {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonDependencyBlocked, lines[0])
		}
		// The deferral must name the untakeable head(s) — the blocked row
		// ids in board order.
		if got := admitField(lines[0], "row"); got != "DEP-A,DEP-B" {
			t.Fatalf("row = %q, want %q (the blocked rows must be named): %s", got, "DEP-A,DEP-B", lines[0])
		}
		// The deferral means NO session: the sim spawner records every spawn.
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none (deferred, not dispatched)", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonDependencyBlocked] != 1 {
			t.Errorf("admission_counters[dependency_blocked] = %d, want 1 (%v)", counters[AdmissionReasonDependencyBlocked], counters)
		}
		if n := countDeferralsFor(t, db, "gap1658-tasks-dep", AdmissionReasonDependencyBlocked); n != 1 {
			t.Errorf("deferrals rows with reason dependency_blocked for gap1658-tasks-dep = %d, want 1", n)
		}
	})

	t.Run("cooldown builder with all pending rows dependency-blocked defers", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t,
			`{"id":"DEP-C","status":"pending","depends_on":["GONE-1"]}`,
			`{"id":"GONE-1","status":"in_progress"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-cool-dep", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd, // cooldown mode (default) + builder class
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-cool-dep")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonDependencyBlocked {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonDependencyBlocked, lines[0])
		}
		if got := admitField(lines[0], "row"); got != "DEP-C" {
			t.Fatalf("row = %q, want DEP-C: %s", got, lines[0])
		}
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none", got)
		}
	})

	t.Run("one free pending row defeats the gate (AC 3)", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t,
			`{"id":"BLOCKED-1","status":"pending","depends_on":["MISSING-1"]}`,
			`{"id":"FREE-1","status":"pending","title":"real work"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-mixed", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-mixed")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (a free pending row is real dispatchable work): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1658-mixed" {
			t.Errorf("simulated selection = %v, want [gap1658-mixed]", got)
		}
	})

	t.Run("depends_on naming a complete row is free (AC 4, order-independent)", func(t *testing.T) {
		db := newTestDB(t)
		// DONE-1 deliberately appears AFTER its dependent: the verdict must
		// not depend on board row order.
		wd := admitWorkdirWithBoard(t,
			`{"id":"DEP-OK","status":"pending","depends_on":["DONE-1"]}`,
			`{"id":"DONE-1","status":"complete"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-dep-ok", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-dep-ok")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (a complete dep satisfies the edge): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1658-dep-ok" {
			t.Errorf("simulated selection = %v, want [gap1658-dep-ok]", got)
		}
	})
}

// TestSCHEDGAP1658_SiblingHeldDefers is AC 2: a BUILDER lane whose pending
// rows are ALL explicitly claimed/in_progress by a sibling (worker_status
// field) defers with reason sibling_held; a worker_status WITHOUT an
// explicit claim spelling is not held (fail-open — ordinary open work).
func TestSCHEDGAP1658_SiblingHeldDefers(t *testing.T) {
	now := fixedEvalNow()

	t.Run("tasks builder with all pending rows sibling-held defers", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t,
			`{"id":"HELD-1","status":"pending","worker_status":"claimed"}`,
			`{"id":"HELD-2","status":"pending","worker_status":"in_progress"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-tasks-held", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-tasks-held")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines for gap1658-tasks-held = %d, want exactly 1: %v", len(lines), lines)
		}
		admitAssertLineShape(t, lines[0])
		if got := admitField(lines[0], "reason"); got != AdmissionReasonSiblingHeld {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonSiblingHeld, lines[0])
		}
		if got := admitField(lines[0], "row"); got != "HELD-1,HELD-2" {
			t.Fatalf("row = %q, want %q (the held rows must be named): %s", got, "HELD-1,HELD-2", lines[0])
		}
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none (deferred, not dispatched)", got)
		}
		if counters := l.AdmissionCounters(); counters[AdmissionReasonSiblingHeld] != 1 {
			t.Errorf("admission_counters[sibling_held] = %d, want 1 (%v)", counters[AdmissionReasonSiblingHeld], counters)
		}
		if n := countDeferralsFor(t, db, "gap1658-tasks-held", AdmissionReasonSiblingHeld); n != 1 {
			t.Errorf("deferrals rows with reason sibling_held for gap1658-tasks-held = %d, want 1", n)
		}
	})

	t.Run("cooldown builder with all pending rows sibling-held defers", func(t *testing.T) {
		db := newTestDB(t)
		wd := admitWorkdirWithBoard(t,
			`{"id":"HELD-C","status":"pending","worker_status":"in-progress"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-cool-held", CooldownS: 60, Last: now.Add(-90 * time.Second),
			Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-cool-held")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonSiblingHeld {
			t.Fatalf("reason = %q, want %q: %s", got, AdmissionReasonSiblingHeld, lines[0])
		}
		if got := simSelectedProjects(t, db); len(got) != 0 {
			t.Errorf("spawned %v, want none", got)
		}
	})

	t.Run("worker_status without an explicit claim spelling is not held", func(t *testing.T) {
		db := newTestDB(t)
		// "pending" (the legacy spelling) is NOT a sibling claim: the row
		// stays ordinary open work and the lane dispatches (fail-open).
		wd := admitWorkdirWithBoard(t,
			`{"id":"LEGACY-1","status":"pending","worker_status":"pending"}`,
		)
		admitInsertProject(t, db, admitProjectSpec{
			Name: "gap1658-ws-legacy", CooldownS: 60, Last: now.Add(-90 * time.Second),
			AdmissionMode: "tasks", Workdir: wd,
		})
		l := admitNewLoop(t, db, now)
		cap := admitCaptureLog(t)

		l.evaluate()

		lines := cap.projectLines("gap1658-ws-legacy")
		if len(lines) != 1 {
			t.Fatalf("ADMIT lines = %d, want exactly 1: %v", len(lines), lines)
		}
		if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
			t.Fatalf("reason = %q, want %q (worker_status=pending is not a sibling claim): %s", got, AdmissionReasonOK, lines[0])
		}
		got := simSelectedProjects(t, db)
		if len(got) != 1 || got[0] != "gap1658-ws-legacy" {
			t.Errorf("simulated selection = %v, want [gap1658-ws-legacy]", got)
		}
	})
}

// TestSCHEDGAP1658_MissingBoardFailsOpen is AC 5: a missing board file is
// an evidence gap — the lane dispatches (fail-open, never starve).
func TestSCHEDGAP1658_MissingBoardFailsOpen(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	wd := t.TempDir() // no .coding-hermes/board at all
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1658-noboard", CooldownS: 60, Last: now.Add(-90 * time.Second),
		AdmissionMode: "tasks", Workdir: wd,
	})
	l := admitNewLoop(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("gap1658-noboard")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for gap1658-noboard = %d, want exactly 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonOK {
		t.Fatalf("reason = %q, want %q (missing board → fail-open): %s", got, AdmissionReasonOK, lines[0])
	}
	got := simSelectedProjects(t, db)
	if len(got) != 1 || got[0] != "gap1658-noboard" {
		t.Errorf("simulated selection = %v, want [gap1658-noboard]", got)
	}
	if counters := l.AdmissionCounters(); counters[AdmissionReasonDependencyBlocked] != 0 || counters[AdmissionReasonSiblingHeld] != 0 {
		t.Errorf("blocked-head counters = %d/%d, want 0/0 (fail-open)", counters[AdmissionReasonDependencyBlocked], counters[AdmissionReasonSiblingHeld])
	}
}

// TestSCHEDGAP1658_StalePremisePriority is AC 6: the SCHED-GAP-1657
// stale-premise gate keeps priority — a board whose pending rows are all
// stale (commit_hash present and landed) reports stale_premise even when a
// row is ALSO blocked, and the blocked-head counters stay 0 (no
// double-defer in one pass).
func TestSCHEDGAP1658_StalePremisePriority(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	wd, sha := gitInitWorkdir(t)
	admitWriteBoard(t, wd,
		fmt.Sprintf(`{"id":"S-STALE","status":"pending","commit_hash":"%s"}`, sha),
		fmt.Sprintf(`{"id":"S-BOTH","status":"pending","commit_hash":"%s","depends_on":["NEVER-1"]}`, sha),
	)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1658-stale-first", CooldownS: 60, Last: now.Add(-90 * time.Second),
		AdmissionMode: "tasks", Workdir: wd,
	})
	l := admitNewLoop(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("gap1658-stale-first")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for gap1658-stale-first = %d, want exactly 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonStalePremise {
		t.Fatalf("reason = %q, want %q (stale keeps priority over the blocked-head gate): %s", got, AdmissionReasonStalePremise, lines[0])
	}
	if counters := l.AdmissionCounters(); counters[AdmissionReasonDependencyBlocked] != 0 || counters[AdmissionReasonSiblingHeld] != 0 {
		t.Errorf("blocked-head counters = %d/%d, want 0/0 (stale gate answered first — no double-defer)",
			counters[AdmissionReasonDependencyBlocked], counters[AdmissionReasonSiblingHeld])
	}
	if got := simSelectedProjects(t, db); len(got) != 0 {
		t.Errorf("spawned %v, want none (deferred by the stale-premise gate)", got)
	}
}

// TestSCHEDGAP1658_NamespacePathGate pins the same gate on the NAMESPACE
// packer path (packer_select.go) — a tasks-mode builder in a namespace
// defers dependency_blocked, and is admitted the moment a free row lands.
func TestSCHEDGAP1658_NamespacePathGate(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	admitInsertNamespace(t, db, "gap1658ns", 0, "cooldown")
	wd := admitWorkdirWithBoard(t,
		`{"id":"NS-DEP","status":"pending","depends_on":["NS-BLOCKER"]}`,
		`{"id":"NS-BLOCKER","status":"in_progress"}`,
	)
	admitInsertProject(t, db, admitProjectSpec{
		Name: "gap1658-ns-dep", NS: "gap1658ns", CooldownS: 60, Last: now.Add(-90 * time.Second),
		AdmissionMode: "tasks", Workdir: wd,
	})
	admitInsertCompletedTick(t, db, "tick-ns-dep-1", "gap1658-ns-dep", now.Add(-90*time.Second))
	l := admitNewLoopNS(t, db, now)
	cap := admitCaptureLog(t)

	l.evaluate()

	lines := cap.projectLines("gap1658-ns-dep")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for gap1658-ns-dep = %d, want exactly 1: %v", len(lines), lines)
	}
	if got := admitField(lines[0], "reason"); got != AdmissionReasonDependencyBlocked {
		t.Fatalf("reason = %q, want %q (namespace packer path): %s", got, AdmissionReasonDependencyBlocked, lines[0])
	}
	if got := simSelectedProjects(t, db); len(got) != 0 {
		t.Errorf("spawned %v, want none (namespace path deferred the blocked builder)", got)
	}
}

// TestSCHEDGAP1658_VocabularyComplete pins that BOTH new reasons are IN
// the frozen vocabulary and are zero-seeded by the counters.
func TestSCHEDGAP1658_VocabularyComplete(t *testing.T) {
	for _, reason := range []string{AdmissionReasonDependencyBlocked, AdmissionReasonSiblingHeld} {
		if !admissionReasonIsKnown(reason) {
			t.Fatalf("reason %q is not in the frozen vocabulary", reason)
		}
		found := false
		for _, r := range admissionReasonVocabulary {
			if r == reason {
				found = true
			}
		}
		if !found {
			t.Fatalf("admissionReasonVocabulary missing %s: %v", reason, admissionReasonVocabulary)
		}
	}
	// A boot-fresh loop must zero-seed both counters.
	l := NewLoop(newTestDB(t), 30*time.Second, 24*time.Hour, 10, 100, 4)
	counters := l.AdmissionCounters()
	if got := counters[AdmissionReasonDependencyBlocked]; got != 0 {
		t.Errorf("admission_counters[dependency_blocked] = %d on a boot-fresh loop, want 0", got)
	}
	if got := counters[AdmissionReasonSiblingHeld]; got != 0 {
		t.Errorf("admission_counters[sibling_held] = %d on a boot-fresh loop, want 0", got)
	}
}

// TestSCHEDGAP1658_GateTable pins the gate's conjunction at the function
// level: builder class × mode × ownership × board shape. Every
// non-blocking shape must be transparent, and the reason must be the
// winning vocabulary entry.
func TestSCHEDGAP1658_GateTable(t *testing.T) {
	allBlocked := admitWorkdirWithBoard(t,
		`{"id":"T-DEP","status":"pending","depends_on":["T-GONE"]}`,
		`{"id":"T-GONE","status":"in_progress"}`,
	)
	allHeld := admitWorkdirWithBoard(t,
		`{"id":"T-HELD","status":"pending","worker_status":"claimed"}`,
	)
	withFree := admitWorkdirWithBoard(t,
		`{"id":"T-DEP","status":"pending","depends_on":["T-GONE"]}`,
		`{"id":"T-GONE","status":"in_progress"}`,
		`{"id":"T-FREE","status":"pending"}`,
	)
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
		wantID    string
	}{
		{"tasks builder all-blocked → blocked, rows named", "proj", allBlocked, "tasks", "", true, "T-DEP"},
		{"cooldown builder all-blocked → blocked, rows named", "proj", allBlocked, "cooldown", "", true, "T-DEP"},
		{"tasks builder all-held → blocked (sibling_held)", "proj", allHeld, "tasks", "", true, "T-HELD"},
		{"cooldown builder all-held → blocked (sibling_held)", "proj", allHeld, "cooldown", "", true, "T-HELD"},
		{"tasks builder one free row → proceeds", "proj", withFree, "tasks", "", false, ""},
		{"tasks builder empty board → proceeds (not this shape)", "proj", emptyBoard, "tasks", "", false, ""},
		{"reporter all-blocked → proceeds (timer by design)", "proj-sync", allBlocked, "tasks", "", false, ""},
		{"tasks builder foreign board → proceeds (ownership refused)", "proj", allBlocked, "tasks", "shared", false, ""},
		{"tasks builder no board → proceeds (fail-open)", "proj", t.TempDir(), "tasks", "", false, ""},
		{"tasks builder empty workdir → proceeds (no evidence)", "proj", "", "tasks", "", false, ""},
		{"markdown board → proceeds (no evidence fields)", "proj", markdown, "tasks", "", false, ""},
		{"unknown mode → proceeds (never widen the gate)", "proj", allBlocked, "bogus", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocked, reason, rowID := premiseBlockedQuiet(tc.project, tc.workdir, tc.mode, tc.ownership, "")
			if blocked != tc.wantBlock {
				t.Errorf("premiseBlockedQuiet(%q, wd set, mode=%q, own=%q) blocked = %v, want %v",
					tc.project, tc.mode, tc.ownership, blocked, tc.wantBlock)
			}
			if !blocked {
				return
			}
			if rowID != tc.wantID {
				t.Errorf("rowID = %q, want %q", rowID, tc.wantID)
			}
			// The reason must be one of the two new vocabulary entries and
			// must agree with the shape (held rows dominate → sibling_held).
			wantReason := AdmissionReasonDependencyBlocked
			if strings.Contains(rowID, "T-HELD") && !strings.Contains(rowID, "T-DEP") {
				wantReason = AdmissionReasonSiblingHeld
			}
			if reason != wantReason {
				t.Errorf("reason = %q, want %q", reason, wantReason)
			}
		})
	}
}

// TestPremiseBoardShapeOf pins the scan's classification contract at the
// shape level: order-independence of the complete-id set, malformed deps
// fail open per row, the narrow completion vocabulary, deferred rows
// excluded from evidence, missing dep rows block, and held rows are
// disjoint from blocked candidates.
func TestPremiseBoardShapeOf(t *testing.T) {
	t.Run("dep row later in the file still satisfies the edge", func(t *testing.T) {
		wd := admitWorkdirWithBoard(t,
			`{"id":"LATE-DEP","status":"pending","depends_on":["LATE-DONE"]}`,
			`{"id":"LATE-DONE","status":"complete"}`,
		)
		shape, ok := premiseBoardShapeOf(wd)
		if !ok {
			t.Fatal("premiseBoardShapeOf = !ok, want ok")
		}
		if shape.pendingCount != 1 || len(shape.blockedIDs) != 0 {
			t.Errorf("shape = %+v, want 1 pending / 0 blocked", shape)
		}
	})

	t.Run("malformed depends_on fails open (row is free)", func(t *testing.T) {
		wd := admitWorkdirWithBoard(t,
			`{"id":"MAL-1","status":"pending","depends_on":"not-an-array"}`,
		)
		shape, ok := premiseBoardShapeOf(wd)
		if !ok {
			t.Fatal("premiseBoardShapeOf = !ok, want ok")
		}
		if shape.pendingCount != 1 || len(shape.blockedIDs) != 0 {
			t.Errorf("shape = %+v, want 1 pending / 0 blocked (malformed deps are not evidence)", shape)
		}
	})

	t.Run("legacy completion spellings do not satisfy a dep edge", func(t *testing.T) {
		// The gate's completion vocabulary is exactly complete /
		// complete-vocabulary (the row's contract); "done" is a
		// board_freshness.go legacy spelling and must NOT satisfy an edge
		// here — the deferral line names the blocker row instead.
		wd := admitWorkdirWithBoard(t,
			`{"id":"LEG-DEP","status":"pending","depends_on":["LEG-DONE"]}`,
			`{"id":"LEG-DONE","status":"done"}`,
		)
		shape, ok := premiseBoardShapeOf(wd)
		if !ok {
			t.Fatal("premiseBoardShapeOf = !ok, want ok")
		}
		if shape.pendingCount != 1 || len(shape.blockedIDs) != 1 || shape.blockedIDs[0] != "LEG-DEP" {
			t.Errorf("shape = %+v, want LEG-DEP blocked (done is not complete)", shape)
		}
	})

	t.Run("deferred dep row is not completion evidence", func(t *testing.T) {
		wd := admitWorkdirWithBoard(t,
			`{"id":"DF-DEP","status":"pending","depends_on":["DF-BLOCKER"]}`,
			`{"id":"DF-BLOCKER","status":"pending","deferred":true}`,
		)
		shape, ok := premiseBoardShapeOf(wd)
		if !ok {
			t.Fatal("premiseBoardShapeOf = !ok, want ok")
		}
		// The deferred row is excluded from the scan entirely (not a
		// candidate, not complete evidence) — the dep stays unsatisfied.
		if shape.pendingCount != 1 || len(shape.blockedIDs) != 1 || shape.blockedIDs[0] != "DF-DEP" {
			t.Errorf("shape = %+v, want DF-DEP blocked (a deferred dep is not complete)", shape)
		}
	})

	t.Run("missing dep row blocks", func(t *testing.T) {
		wd := admitWorkdirWithBoard(t,
			`{"id":"GHOST-DEP","status":"pending","depends_on":["NOT-ON-BOARD"]}`,
		)
		shape, ok := premiseBoardShapeOf(wd)
		if !ok {
			t.Fatal("premiseBoardShapeOf = !ok, want ok")
		}
		if shape.pendingCount != 1 || len(shape.blockedIDs) != 1 || shape.blockedIDs[0] != "GHOST-DEP" {
			t.Errorf("shape = %+v, want GHOST-DEP blocked (a dep absent from the board is not complete)", shape)
		}
	})

	t.Run("held rows are disjoint from blocked candidates", func(t *testing.T) {
		wd := admitWorkdirWithBoard(t,
			`{"id":"HELD-AND-BLOCKED-SHAPE","status":"pending","worker_status":"claimed","depends_on":["X"]}`,
		)
		shape, ok := premiseBoardShapeOf(wd)
		if !ok {
			t.Fatal("premiseBoardShapeOf = !ok, want ok")
		}
		if shape.pendingCount != 1 {
			t.Fatalf("pendingCount = %d, want 1", shape.pendingCount)
		}
		if len(shape.heldIDs) != 1 || len(shape.blockedIDs) != 0 {
			t.Errorf("shape = %+v, want the row counted held exactly once (never also blocked)", shape)
		}
	})

	t.Run("missing board fails open", func(t *testing.T) {
		if _, ok := premiseBoardShapeOf(t.TempDir()); ok {
			t.Error("premiseBoardShapeOf(no board) = ok, want false (fail-open)")
		}
		if _, ok := premiseBoardShapeOf(""); ok {
			t.Error("premiseBoardShapeOf(empty workdir) = ok, want false (fail-open)")
		}
	})
}

// TestPremiseBlockedQuietReporterExempt pins the reporter exemption once
// more at the gate level with a REAL all-blocked board (the -sync suffix
// family keeps its timer cadence BY DESIGN, SCHED-GAP-1655 deliverable 3).
func TestPremiseBlockedQuietReporterExempt(t *testing.T) {
	wd := admitWorkdirWithBoard(t,
		`{"id":"R-DEP","status":"pending","depends_on":["R-GONE"]}`,
	)
	if blocked, _, _ := premiseBlockedQuiet("fleet-sync", wd, "cooldown", "", ""); blocked {
		t.Error("reporter lane blocked on an all-blocked board — reporter cadence is BY DESIGN")
	}
}

// TestPremiseBlockedQuietBlockedRowIDs pins the detail contract: the row
// ids are comma-joined in board order, blocked rows first, and the value
// is safe for the ADMIT line's row= field (no whitespace).
func TestPremiseBlockedQuietBlockedRowIDs(t *testing.T) {
	wd := admitWorkdirWithBoard(t,
		`{"id":"ID-2","status":"pending","depends_on":["Z"]}`,
		`{"id":"ID-1","status":"pending","depends_on":["Z"]}`,
		`{"id":"Z","status":"in_progress"}`,
	)
	blocked, reason, rowID := premiseBlockedQuiet("proj", wd, "cooldown", "", "")
	if !blocked {
		t.Fatal("blocked = false, want true")
	}
	if reason != AdmissionReasonDependencyBlocked {
		t.Errorf("reason = %q, want %q", reason, AdmissionReasonDependencyBlocked)
	}
	if rowID != "ID-2,ID-1" {
		t.Errorf("rowID = %q, want %q (board order)", rowID, "ID-2,ID-1")
	}
	if strings.ContainsAny(rowID, " 	\n") {
		t.Errorf("rowID %q contains whitespace — it would break the row= field on the ADMIT line", rowID)
	}
}
