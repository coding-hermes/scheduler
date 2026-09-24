package scheduler

// SCHED-GAP-1597 — the declared-but-never-written ticks columns.
//
// The production audit (scheduler.db, 7 days, 2,877 rows) measured:
//
//	urgency      0 non-empty     weight_used  0 non-empty
//	slot_wait_ms 2 non-zero      admit_reason 50 non-empty
//	exit_code    0 non-zero (and 2879 zero)  — every gateway-completed,
//	             spawn-failure and shutdown-drain row carried a fabricated 0
//
// despite writers existing for all of them. Root causes pinned by these tests:
//
//  1. ORDERING: SlotPool.spawn stamped the admission row BEFORE
//     lifecycle.Enqueue created it — the stamp's UPDATE matched 0 rows and was
//     silently lost on the packer path. Only the pre-enqueued resume/nudge
//     rows were ever stamped (the 50/74 admit_reason rows in the audit).
//     TestSCHEDGAP1597_SpawnPathEnqueuesBeforeStamping runs the Spawn (not
//     SpawnEnqueued) entry point, the exact shape that was broken, and
//     TestSCHEDGAP1597_PackerSpawnPersistsUrgencyWeightAndAdmission pins the
//     SpawnEnqueued ordering.
//  2. MISSING SELECTION FACTS: the INSERT omitted urgency/weight_used, and no
//     production writer ever set them. The stamp now carries the packer's
//     computed urgency and effective weight (PackedProject.Urgency/.Weight) at
//     the admit boundary.
//  3. FABRICATED EXIT CODE: TickOutcome's zero-value ExitCode (0) was written
//     for ticks where no process exit ever existed. The no-process sites now
//     state -1 (Complete persists -1 as SQL NULL) and the gateway-completed
//     site states its 0 convention explicitly.
//
// The guard test asserts the acceptance: after one real spawn→completion
// cycle, NONE of the five declared columns reads as defaulted.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// schedGap1597Facts reads the SCHED-GAP-1597 admission facts of one tick row.
func schedGap1597Facts(t *testing.T, db *sql.DB, tickID string) (urgency float64, weightUsed int, slotWaitMs int64, admitReason string) {
	t.Helper()
	err := db.QueryRow(
		`SELECT urgency, weight_used, slot_wait_ms, admit_reason FROM ticks WHERE id = ?`, tickID).
		Scan(&urgency, &weightUsed, &slotWaitMs, &admitReason)
	if err != nil {
		t.Fatalf("read SCHED-GAP-1597 facts for tick %s: %v", tickID, err)
	}
	return urgency, weightUsed, slotWaitMs, admitReason
}

// schedGap1597TerminalFacts reads the terminal honesty columns of one tick
// row. exitCode is any so NULL (no process exit) is distinguishable from 0.
func schedGap1597TerminalFacts(t *testing.T, db *sql.DB, tickID string) (exitCode any, status string) {
	t.Helper()
	err := db.QueryRow(`SELECT exit_code, status FROM ticks WHERE id = ?`, tickID).Scan(&exitCode, &status)
	if err != nil {
		t.Fatalf("read terminal facts for tick %s: %v", tickID, err)
	}
	return exitCode, status
}

// TestSCHEDGAP1597_PackerSpawnPersistsUrgencyWeightAndAdmission drives the
// REAL pool through SpawnEnqueued (the packer entry point) and pins that the
// admit stamp lands ON the row with the packer's selection facts: urgency and
// effective weight exactly as selected, the "ok" admission decision, and the
// measured (sim-clock) slot wait. Under the pre-1597 ordering this test
// failed: the stamp ran before the row existed and every value read as the
// column default.
func TestSCHEDGAP1597_PackerSpawnPersistsUrgencyWeightAndAdmission(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)

	gw := newHeldResumeGateway(t)
	gw.wire(l)

	const proj = "1597-packed"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})

	// The packer's selection: a computed urgency and the effective weight it
	// allocated. SpawnEnqueued is the packer entry point.
	tickID := database.NextTickID(clock.WithClock(context.Background(), sim), proj)
	if err := l.lifecycle.Enqueue(proj, tickID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	l.slotPool.SpawnEnqueued(PackedProject{
		Name:        proj,
		NamespaceID: "",
		Urgency:     22.5,
		Weight:      7,
	}, tickID, sim.Now(), true, db)

	// The spawn goroutine must reach the gateway before the row is judged —
	// the stamp runs BEFORE the gateway POST, so a settled admit stamp is the
	// thing under test and the held gateway keeps the row running.
	waitForHeldSpawn(t, gw, 10*time.Second)

	urgency, weightUsed, slotWaitMs, admitReason := schedGap1597Facts(t, db, tickID)
	if urgency != 22.5 {
		t.Errorf("ticks.urgency = %v, want 22.5 — the packer's computed urgency at selection time must be persisted (pre-1597 this column read 0 on every production row)", urgency)
	}
	if weightUsed != 7 {
		t.Errorf("ticks.weight_used = %d, want 7 — the effective weight the packer allocated must be persisted (pre-1597 this column read 0 on every production row)", weightUsed)
	}
	if admitReason != AdmissionReasonOK {
		t.Errorf("ticks.admit_reason = %q, want %q — the admit stamp must land on the row (pre-1597 the packer path stamped before the row existed, so the stamp was silently lost)", admitReason, AdmissionReasonOK)
	}
	if slotWaitMs != 0 {
		t.Errorf("ticks.slot_wait_ms = %d, want 0 — the slot was free and the sim clock never advanced between waitStart and acquire", slotWaitMs)
	}

	// Release and settle: the completion path must NOT clobber the stamp.
	gw.releaseAll()
	waitForSettled(t, db, 10*time.Second)

	urgency2, weightUsed2, _, admitReason2 := schedGap1597Facts(t, db, tickID)
	if urgency2 != 22.5 || weightUsed2 != 7 || admitReason2 != AdmissionReasonOK {
		t.Errorf("after completion: urgency=%v weight=%d admit=%q — lifecycle.Complete must not clobber the admission stamp",
			urgency2, weightUsed2, admitReason2)
	}
}

// TestSCHEDGAP1597_SpawnPathEnqueuesBeforeStamping is the RED proof for the
// root cause: the SlotPool.Spawn entry point (the row does NOT pre-exist —
// the pool itself enqueues it) must still carry the full admit stamp. Under
// the pre-1597 ordering this failed exactly the way production did: the stamp
// UPDATE matched 0 rows and the row read as never-written.
func TestSCHEDGAP1597_SpawnPathEnqueuesBeforeStamping(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)

	gw := newHeldResumeGateway(t)
	gw.wire(l)

	const proj = "1597-spawnpath"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})

	// Spawn (not SpawnEnqueued): the pool generates the id and creates the
	// row itself.
	tickID := l.slotPool.Spawn(PackedProject{
		Name:        proj,
		NamespaceID: "",
		Urgency:     11.25,
		Weight:      3,
	}, sim.Now(), true, db)
	if tickID == "" {
		t.Fatal("Spawn returned an empty tick id")
	}

	waitForHeldSpawn(t, gw, 10*time.Second)

	urgency, weightUsed, _, admitReason := schedGap1597Facts(t, db, tickID)
	if admitReason != AdmissionReasonOK {
		t.Errorf("ticks.admit_reason = %q, want %q — the Spawn path must enqueue BEFORE stamping (pre-1597 the stamp preceded the row and was lost)", admitReason, AdmissionReasonOK)
	}
	if urgency != 11.25 {
		t.Errorf("ticks.urgency = %v, want 11.25 — the selection facts must survive the pool-side enqueue", urgency)
	}
	if weightUsed != 3 {
		t.Errorf("ticks.weight_used = %d, want 3 — the selection facts must survive the pool-side enqueue", weightUsed)
	}

	gw.releaseAll()
	waitForSettled(t, db, 10*time.Second)
}

// TestSCHEDGAP1597_ManualAndResumeSpawnsCarryZeroSelectionFacts pins the
// honest-empty convention: a tick NO packer selected (a manual/resume spawn —
// PackedProject built without a packer pass) records 0/0 WITH a stamped
// admit_reason, so 0 reads as "no packer selection", never as "column not
// written". The admit_reason presence is what distinguishes stamped-zero from
// never-stamped — the same convention nudge_source uses.
func TestSCHEDGAP1597_ManualAndResumeSpawnsCarryZeroSelectionFacts(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)

	gw := newHeldResumeGateway(t)
	gw.wire(l)

	const proj = "1597-manual"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})

	tickID := database.NextTickID(clock.WithClock(context.Background(), sim), proj)
	if err := l.lifecycle.Enqueue(proj, tickID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// No packer ran: urgency/weight stay at their zero values.
	l.slotPool.SpawnEnqueued(PackedProject{Name: proj, NamespaceID: ""}, tickID, sim.Now(), true, db)

	waitForHeldSpawn(t, gw, 10*time.Second)

	urgency, weightUsed, _, admitReason := schedGap1597Facts(t, db, tickID)
	if admitReason != AdmissionReasonOK {
		t.Fatalf("ticks.admit_reason = %q, want %q — the stamp must land even without packer facts", admitReason, AdmissionReasonOK)
	}
	if urgency != 0 || weightUsed != 0 {
		t.Errorf("urgency=%v weight_used=%d, want 0/0 — a tick no packer selected must read as an honest zero, not a fabricated selection", urgency, weightUsed)
	}

	gw.releaseAll()
	waitForSettled(t, db, 10*time.Second)
}

// TestSCHEDGAP1597_SpawnFailureOutcomeWritesNullExitCode is the RED proof for
// the fabricated exit_code=0: a tick whose spawn fails before any process
// exists (nil gateway, exec fallback disabled) must persist exit_code NULL —
// Complete's -1→NULL convention — not the struct-default 0. In the 7-day
// audit every "gateway unreachable" row carried a fabricated 0.
func TestSCHEDGAP1597_SpawnFailureOutcomeWritesNullExitCode(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)

	const proj = "1597-nogateway"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})

	// No gateway client wired + exec fallback disabled → Spawn fails at the
	// spawner with "gateway unreachable …", slot_pool completes the row as
	// failed through lifecycle.Complete.
	l.spawner.SetNoExecFallback(true)

	tickID := l.slotPool.Spawn(PackedProject{Name: proj, NamespaceID: ""}, sim.Now(), true, db)
	if tickID == "" {
		t.Fatal("Spawn returned an empty tick id")
	}

	waitUntil(t, 10*time.Second, "the failed tick to reach a terminal status", func() bool {
		st := schedGap157TickStatus(t, db, tickID)
		return st == "failed" || st == "timeout"
	})

	exitCode, status := schedGap1597TerminalFacts(t, db, tickID)
	if status != "failed" {
		t.Fatalf("status = %q, want failed — the spawn was refused, the row must be a failure", status)
	}
	if exitCode != nil {
		t.Errorf("ticks.exit_code = %v, want NULL — no process ever ran, so there is no exit status (pre-1597 this row carried a fabricated 0)", exitCode)
	}
}

// TestSCHEDGAP1597_CompletePersistsNegativeOneAsNULL pins the convention the
// no-process completion sites now rely on: TickOutcome.ExitCode -1 reaches the
// column as SQL NULL (the "no process exit to record" state), while a real
// non-zero process code is preserved verbatim.
func TestSCHEDGAP1597_CompletePersistsNegativeOneAsNULL(t *testing.T) {
	db := newTestDB(t)
	lt := NewLifecycleTracker(db)

	const proj = "1597-complete-null"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})

	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	tickID := database.NextTickID(clock.WithClock(context.Background(), clock.NewManualSimClock(now)), proj)
	if err := lt.Enqueue(proj, tickID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// -1 (no process) → NULL.
	if err := lt.Complete(TickOutcome{
		TickID:   tickID,
		Project:  proj,
		Started:  now,
		Finished: now.Add(time.Minute),
		Status:   TickFailed,
		ExitCode: -1,
		Error:    "gateway refused",
	}); err != nil {
		t.Fatalf("Complete(-1): %v", err)
	}
	var exitCode any
	if err := db.QueryRow(`SELECT exit_code FROM ticks WHERE id = ?`, tickID).Scan(&exitCode); err != nil {
		t.Fatalf("read exit_code: %v", err)
	}
	if exitCode != nil {
		t.Errorf("exit_code for a -1 outcome = %v, want NULL — -1 is the no-process convention, never a stored code", exitCode)
	}

	// A real process code is preserved verbatim. A second dormant clock at a
	// different instant keeps the NextTickID timestamps unique.
	now2 := now.Add(time.Hour)
	tickID2 := database.NextTickID(clock.WithClock(context.Background(), clock.NewManualSimClock(now2)), proj)
	if err := lt.Enqueue(proj, tickID2); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if err := lt.Complete(TickOutcome{
		TickID:   tickID2,
		Project:  proj,
		Started:  now,
		Finished: now.Add(time.Minute),
		Status:   TickFailed,
		ExitCode: 2,
		Error:    "exit status 2",
	}); err != nil {
		t.Fatalf("Complete(2): %v", err)
	}
	var real int
	if err := db.QueryRow(`SELECT exit_code FROM ticks WHERE id = ?`, tickID2).Scan(&real); err != nil {
		t.Fatalf("read exit_code 2: %v", err)
	}
	if real != 2 {
		t.Errorf("exit_code for a real process code = %d, want 2 — real codes must never be rewritten", real)
	}
}

// TestSCHEDGAP1597_GatewayCompletedTickWritesExplicitZeroExitCode pins the
// gateway-completed convention: a tick the gateway finished (no child process
// to reap) completes with exit_code 0 stated EXPLICITLY in the outcome, and
// lifecycle.Complete persists that 0. The assertion guards the invariant the
// docs table (docs/troubleshooting-scheduling-errors.md §4) documents: 0 on a
// COMPLETED gateway row is the convention, NULL on a no-process FAILURE row is
// the absence of a code.
func TestSCHEDGAP1597_GatewayCompletedTickWritesExplicitZeroExitCode(t *testing.T) {
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	st := &SpawnedTick{
		TickID:     "1597-exit-gw",
		Project:    "1597-exit-gw-proj",
		Started:    start,
		spawner:    NewSpawner(nil, 1),
		completed:  true,
		completeAt: start.Add(30 * time.Second),
		reqStart:   start,
		workdir:    "", // no workdir: countGitChanges no-ops, closure gate no-ops
	}

	out := st.Wait()

	if out.Status != TickCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if out.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want the explicit gateway-completed 0", out.ExitCode)
	}

	// And the writer persists it verbatim (0 IS the convention here —
	// distinctly NOT the -1→NULL no-process state).
	db := newTestDB(t)
	lt := NewLifecycleTracker(db)
	const proj = "1597-exit-gw-proj"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})
	if err := lt.Enqueue(proj, out.TickID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	out.Finished = st.completeAt
	if err := lt.Complete(out); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var exitCode any
	if err := db.QueryRow(`SELECT exit_code FROM ticks WHERE id = ?`, out.TickID).Scan(&exitCode); err != nil {
		t.Fatalf("read exit_code: %v", err)
	}
	if exitCode == nil {
		t.Fatal("exit_code = NULL, want 0 — a gateway-completed row states the convention, not the no-process state")
	}
	if code, ok := exitCode.(int64); !ok || code != 0 {
		t.Errorf("exit_code = %v, want 0", exitCode)
	}
}

// TestSCHEDGAP1597_GuardDeclaredTickColumnsAreWritten is the GUARD: after one
// real spawn→completion cycle through the pool, NONE of the five audited
// columns may read as defaulted. This is the acceptance phrased the way the
// audit measured it — if a future refactor reintroduces a silent writer
// (ordering, omission, or a dead writer), this fails with the column name.
func TestSCHEDGAP1597_GuardDeclaredTickColumnsAreWritten(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 2)
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)

	gw := newHeldResumeGateway(t)
	gw.wire(l)

	const proj = "1597-guard"
	admitInsertProject(t, db, admitProjectSpec{Name: proj})

	tickID := database.NextTickID(clock.WithClock(context.Background(), sim), proj)
	if err := l.lifecycle.Enqueue(proj, tickID); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	l.slotPool.SpawnEnqueued(PackedProject{
		Name:        proj,
		NamespaceID: "",
		Urgency:     5.5,
		Weight:      2,
	}, tickID, sim.Now(), true, db)

	gw.releaseAll()
	waitForSettled(t, db, 10*time.Second)

	var (
		urgency     any
		weightUsed  any
		slotWaitMs  any
		admitReason any
		exitCode    any
	)
	if err := db.QueryRow(`SELECT urgency, weight_used, slot_wait_ms, admit_reason, exit_code FROM ticks WHERE id = ?`, tickID).
		Scan(&urgency, &weightUsed, &slotWaitMs, &admitReason, &exitCode); err != nil {
		t.Fatalf("read guarded columns: %v", err)
	}
	admitReasonS, _ := admitReason.(string)

	if urgency == nil {
		t.Errorf("urgency is NULL — a packer-selected tick must carry its selection-time urgency")
	}
	if u, ok := urgency.(float64); ok && u == 0 {
		t.Errorf("urgency = 0 on a packer-selected tick (5.5 was selected) — the selection facts were not persisted")
	}
	if w, ok := weightUsed.(int64); !ok || w != 2 {
		t.Errorf("weight_used = %v, want the packer-allocated weight 2", weightUsed)
	}
	if admitReasonS == "" {
		t.Errorf("admit_reason is empty — every tick that reached the pool must carry its admission decision")
	}
	if slotWaitMs == nil {
		t.Errorf("slot_wait_ms is NULL — the pool measures the wait even when it is 0")
	}
	if exitCode == nil {
		t.Errorf("exit_code is NULL on a completed gateway tick — the gateway-completed convention is an explicit 0")
	}
}
