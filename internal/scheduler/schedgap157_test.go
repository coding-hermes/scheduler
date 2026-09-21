package scheduler

// SCHED-GAP-157-T1 — acceptance tests for the tick-lifecycle columns landed in
// SCHED-GAP-157 (commit 738c009d): `ticks.slot_wait_ms`, `ticks.admit_reason`,
// `ticks.nudge_source` (migration v35) and the `deferrals` table.
//
// The row's acceptance is two questions that must be answerable from the DB
// alone: "how long did this lane wait for a slot" and "why was it skipped" —
// both previously requiring log-line ORDER across a rotated scheduler.log.
// These tests pin the four acceptance areas:
//
//	(a) SLOT WAIT  — TestSCHEDGAP157_SlotWaitStampExactAndMonotonic (stamp path,
//	                 exact values from the injected clock)
//	                 TestSCHEDGAP157_SlotPoolStampsMeasuredSlotWait (the REAL
//	                 SlotPool.spawn goroutine measures its own wait) plus its
//	                 control TestSCHEDGAP157_SlotPoolStampsZeroForAFreeSlot
//	                 (a free slot records the "never waited" 0)
//	(b) DEFERRALS  — TestSCHEDGAP157_DeferralRoundTripAndValidation,
//	                 TestSCHEDGAP157_EvaluationPassPersistsDeferralRows,
//	                 TestSCHEDGAP157_StarvationQuerySelectsOnlyStarvedLanes
//	(c) LEGACY ROWS— TestSCHEDGAP157_LegacyRowsReadAsDefaultsAfterMigration
//	(d) NUDGE      — TestSCHEDGAP157_NudgeSourceStampedDistinctly,
//	                 TestSCHEDGAP157_SlotPoolStampsAdmitReasonForNudgeTicks
//
// LEVEL OF EACH TEST (stated, per the task brief, because the two slot-wait
// tests deliberately sit at DIFFERENT levels):
//
//   - The stamp-level tests drive stampTickAdmission / database.RecordTickAdmission
//     with a wait MEASURED on the injected clock (slotWait is `Since(waitStart)`
//     produced exactly the way SlotPool.spawn produces it). They assert exact
//     millisecond values, which is what makes the clock the only source of the
//     recorded number.
//   - The pool-level tests drive the REAL SlotPool.spawn goroutine (the same
//     fixture harness the SCHED-GAP-144/146 pool tests use) so the call site —
//     "stamp at the admit → start boundary with p.clock().Since(waitStart)" —
//     is pinned, not just the writer it calls. SlotPool.spawn is fire-and-forget,
//     so these tests observe it through the DB (the row's stamps) with bounded
//     `waitUntil` pre-condition polls, never through a measurement sleep.
//
// DETERMINISM (SCHED-GAP-169): every clock read under test goes through
// internal/clock. The slot waits are driven with clock.NewManualSimClock (or a
// fixed start instant) and moved explicitly with Advance — no time.Sleep is
// ever the thing being measured. The only real-time waits are bounded
// `waitUntil` pre-conditions (the existing package convention), which poll for
// a goroutine to *reach* a point, not for an amount of time to pass.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
	"github.com/coding-hermes/scheduler/internal/database"
)

// schedGap157StarvedLane is one row of the acceptance starvation report.
type schedGap157StarvedLane struct {
	project string
	ticks   int
	maxWait int64 // ms
}

// schedGap157StarvationQuery is the acceptance STARVATION QUERY (acceptance
// (b)): ONE query over `ticks` that answers "which lanes waited longer for a
// slot than the pool's own patience" — the intent of the Ticks section of
// docs/api.md (§7 in the shipped doc; §7.1 in the SCHED-GAP-157-T1 brief).
//
// EXACT SQL (the `?` is StarvationThreshold.Milliseconds() = 300000):
//
//	SELECT project_name,
//	       COUNT(*)          AS waited_ticks,
//	       MAX(slot_wait_ms) AS max_slot_wait_ms
//	FROM ticks
//	WHERE slot_wait_ms > ?
//	GROUP BY project_name
//	ORDER BY max_slot_wait_ms DESC, project_name
//
// The predicate is STRICT (>): a lane that waited exactly the threshold did
// not wait PAST it. StarvationThreshold is 5 minutes, the slot pool's own
// default patience (slot_pool.go: defaultSlotPatience) — a lane that waited
// past the point where the pool drops work for other lanes is the definition
// of a starved lane.
const schedGap157StarvationQuery = `
SELECT project_name, COUNT(*) AS waited_ticks, MAX(slot_wait_ms) AS max_slot_wait_ms
FROM ticks
WHERE slot_wait_ms > ?
GROUP BY project_name
ORDER BY max_slot_wait_ms DESC, project_name`

// schedGap157Stamps reads the three SCHED-GAP-157 columns RAW from the row —
// no COALESCE, so a NULL or a missing column fails the read instead of being
// silently defaulted into a passing zero.
func schedGap157Stamps(t *testing.T, db *sql.DB, tickID string) (slotWaitMs int64, admitReason, nudgeSource string) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT slot_wait_ms, admit_reason, nudge_source FROM ticks WHERE id = ?`, tickID).
		Scan(&slotWaitMs, &admitReason, &nudgeSource); err != nil {
		t.Fatalf("read SCHED-GAP-157 stamps for tick %s: %v", tickID, err)
	}
	return slotWaitMs, admitReason, nudgeSource
}

// schedGap157TickStatus returns a tick row's status ("" when the row is absent).
func schedGap157TickStatus(t *testing.T, db *sql.DB, tickID string) string {
	t.Helper()
	var st string
	err := db.QueryRow(`SELECT status FROM ticks WHERE id = ?`, tickID).Scan(&st)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read status of tick %s: %v", tickID, err)
	}
	return st
}

// schedGap157Starvation runs the acceptance starvation query and returns the
// starved lanes newest-style (most-starved first).
func schedGap157Starvation(t *testing.T, db *sql.DB, thresholdMs int64) []schedGap157StarvedLane {
	t.Helper()
	rows, err := db.Query(schedGap157StarvationQuery, thresholdMs)
	if err != nil {
		t.Fatalf("starvation query: %v", err)
	}
	defer rows.Close()
	var out []schedGap157StarvedLane
	for rows.Next() {
		var l schedGap157StarvedLane
		if err := rows.Scan(&l.project, &l.ticks, &l.maxWait); err != nil {
			t.Fatalf("scan starvation row: %v", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate starvation rows: %v", err)
	}
	return out
}

// schedGap157SeedWait stamps one tick row with an exact slot wait, creating the
// project row first when it does not exist yet (ticks.project_name is a FK to
// projects.name with foreign_keys=ON). Idempotent for the project so a lane can
// be seeded with several ticks.
func schedGap157SeedWait(t *testing.T, db *sql.DB, project, tickID string, waitMs int64) {
	t.Helper()
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE name = ?`, project).Scan(&exists); err != nil {
		t.Fatalf("check project %s: %v", project, err)
	}
	if exists == 0 {
		admitInsertProject(t, db, admitProjectSpec{Name: project, CooldownS: 21600})
	}
	queuedTickRow(t, db, tickID, project)
	if _, err := db.Exec(`UPDATE ticks SET slot_wait_ms = ? WHERE id = ?`, waitMs, tickID); err != nil {
		t.Fatalf("seed slot_wait_ms=%d on %s: %v", waitMs, tickID, err)
	}
}

// ── (a) SLOT WAIT ─────────────────────────────────────────────────────────

// TestSCHEDGAP157_SlotWaitStampExactAndMonotonic is acceptance (a) at the STAMP
// level: the recorded slot_wait_ms is the duration the caller MEASURED on the
// injected clock (SlotPool.spawn's own expression, p.clock().Since(waitStart)),
// and a second longer wait records a larger value — so the column is a
// measurement, never a constant.
//
// The clock is a dormant simulator: nothing moves it except the test, so the
// exact values below are the clock, not a wall-clock approximation. A tick that
// never waited keeps 0 (the "0 = never waited" contract).
func TestSCHEDGAP157_SlotWaitStampExactAndMonotonic(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const (
		first  = "157-wait-first"
		second = "157-wait-second"
		zeroed = "157-wait-stamped-zero"
		raw    = "157-wait-never-stamped"
	)
	firstID, secondID, zeroedID, rawID := first+"-t1", second+"-t1", zeroed+"-t1", raw+"-t1"
	for _, s := range []struct{ project, tickID string }{
		{first, firstID}, {second, secondID}, {zeroed, zeroedID}, {raw, rawID},
	} {
		admitInsertProject(t, db, admitProjectSpec{Name: s.project, CooldownS: 21600})
		queuedTickRow(t, db, s.tickID, s.project)
	}

	// Dormant simulator, explicitly advanced: every wait below is decided by the
	// test, so the expected millisecond values are exact.
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)

	// This is the pool's own measurement expression (slot_pool.go: waitStart is
	// read before Acquire, Since(waitStart) is taken at the admit boundary).
	waitStart := sim.Now()
	sim.Advance(90 * time.Second)
	stampTickAdmission(db, firstID, sim.Since(waitStart), AdmissionReasonOK, "")

	// A second, longer wait: the second wait is compared against the first for
	// monotonicity, so "always 0" and "always 1ms" both fail here.
	sim.Advance(45 * time.Second)
	stampTickAdmission(db, secondID, sim.Since(waitStart), AdmissionReasonOK, "")

	// A lane that never waited. Stamped with a zero duration (the pool's own
	// value when the slot is free) AND left completely unstamped: both must read
	// back as the column default 0.
	stampTickAdmission(db, zeroedID, 0, AdmissionReasonOK, "")

	wantFirst, wantSecond := int64(90_000), int64(135_000)
	gotFirst, reasonFirst, _ := schedGap157Stamps(t, db, firstID)
	gotSecond, reasonSecond, _ := schedGap157Stamps(t, db, secondID)
	gotZeroed, reasonZeroed, _ := schedGap157Stamps(t, db, zeroedID)
	gotRaw, reasonRaw, _ := schedGap157Stamps(t, db, rawID)

	if gotFirst != wantFirst {
		t.Errorf("slot_wait_ms for the first wait = %d, want exactly %d — the recorded wait must be the duration measured on the injected clock (90s), not a wall-clock reading", gotFirst, wantFirst)
	}
	if gotSecond != wantSecond {
		t.Errorf("slot_wait_ms for the second wait = %d, want exactly %d (the clock was advanced 90s then 45s)", gotSecond, wantSecond)
	}
	if gotSecond < gotFirst {
		t.Errorf("second wait (%d ms) < first wait (%d ms) — a longer wait must never record a smaller value", gotSecond, gotFirst)
	}
	if gotZeroed != 0 {
		t.Errorf("stamped-zero wait = %d, want 0 — a tick whose slot was free records 0 (never waited), never a fabricated value", gotZeroed)
	}
	if gotRaw != 0 {
		t.Errorf("unstamped tick slot_wait_ms = %d, want the column default 0", gotRaw)
	}
	for _, c := range []struct {
		project, got string
	}{
		{first, reasonFirst}, {second, reasonSecond}, {zeroed, reasonZeroed},
	} {
		if c.got != AdmissionReasonOK {
			t.Errorf("admit_reason for %s = %q, want %q — every tick that reached the pool carries the ok decision", c.project, c.got, AdmissionReasonOK)
		}
	}
	// The stamp is the ONLY writer: a row nobody stamped carries no decision at
	// all, which is what distinguishes "never stamped" from "admitted as ok".
	if reasonRaw != "" {
		t.Errorf("unstamped tick admit_reason = %q, want \"\" — nothing may fabricate an admission decision for a row that was never stamped", reasonRaw)
	}

	// The read path the API uses must agree with the raw read (no divergence
	// between the model and the table).
	tk, err := database.GetTick(ctx, db, firstID)
	if err != nil {
		t.Fatalf("GetTick(%s): %v", firstID, err)
	}
	if tk.SlotWaitMs != gotFirst || tk.AdmitReason != reasonFirst {
		t.Errorf("GetTick reports slot_wait_ms=%d admit_reason=%q, want %d/%q (matching the raw row)",
			tk.SlotWaitMs, tk.AdmitReason, gotFirst, reasonFirst)
	}
}

// TestSCHEDGAP157_SlotPoolStampsMeasuredSlotWait is acceptance (a) through the
// REAL pool: a tick whose spawn goroutine must wait for the pool's only free
// slot records the wait the pool measured on its own clock seam.
//
// Shape (the SCHED-GAP-144/146 pool harness):
//   - the pool has exactly ONE global slot and the test holds it (the
//     SCHED-GAP-145/patience pattern of holding a slot with pool.Acquire), so a
//     spawned tick cannot be admitted without waiting;
//   - the parked tick's spawn goroutine claims its namespace slot a handful of
//     instructions BEFORE it reads waitStart (slot_pool.go), which is the
//     barrier this test waits on — a bounded precondition poll on
//     NamespacePending, not a sleep;
//   - virtual time then advances on the dormant simulator and the held slot is
//     released, so the measured wait is a duration only the injected clock
//     produced.
//
// The measurement RETRIES. The barrier narrows the read-vs-advance race to a
// few instructions, but a descheduled goroutine could still read waitStart
// after the last advance — a scheduling artifact, not behaviour of the code
// under test. A pool that does not measure (a constant 0, or a fabricated
// constant) fails every attempt, which is exactly the property under test.
// The zero-wait control for this contract is TestSCHEDGAP157_SlotPoolStampsZeroForAFreeSlot.
func TestSCHEDGAP157_SlotPoolStampsMeasuredSlotWait(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 1) // ONE global slot
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)

	// The gateway is HELD, not mocked-away: a spawned tick parks there until
	// release, so nothing reaches a terminal status before it has been stamped.
	gw := newHeldResumeGateway(t)
	gw.wire(l)

	const holder = "157-slotwait-holder"
	if !l.slotPool.Acquire(context.Background(), holder) {
		t.Fatal("Acquire for the slot holder failed — the parked tick has nothing to wait for")
	}

	var (
		measured     int64
		measuredTick string
	)
	const (
		advanceBefore = 30 * time.Second // the clock step a slow goroutine may land in
		advanceAfter  = 60 * time.Second // the step that must precede the acquire
	)
	for attempt := 1; attempt <= 3; attempt++ {
		ns := fmt.Sprintf("157-slotwait-%d", attempt)
		proj := fmt.Sprintf("157-parked-%d", attempt)
		// cap > 0 so the spawn goroutine takes a namespace claim — that claim is
		// the barrier proving the goroutine has passed the pre-slot-wait
		// bookkeeping and is about to read waitStart.
		capTestNamespace(t, db, ns, 1, "cooldown")
		admitInsertProject(t, db, admitProjectSpec{Name: proj, NS: ns, CooldownS: 21600})

		tickID := database.NextTickID(clock.WithClock(context.Background(), sim), proj)
		queuedTickRow(t, db, tickID, proj)
		l.slotPool.SpawnEnqueued(PackedProject{Name: proj, NamespaceID: ns}, tickID, sim.Now(), true, db)

		waitUntil(t, 10*time.Second, "the parked spawn to claim its namespace slot (barrier before waitStart)", func() bool {
			return l.slotPool.NamespacePending(ns) == 1
		})

		// Move the clock in two steps, then free the slot. Whatever instant the
		// goroutine read waitStart at, the acquire now happens at the final
		// instant charged to the injected clock.
		sim.Advance(advanceBefore)
		sim.Advance(advanceAfter)
		l.slotPool.Release(holder)

		// The stamp lands BEFORE the queued→running transition (slot_pool.go:
		// stampTickAdmission then lifecycle.Enqueue/StartRunning), so waiting for
		// the row to leave 'queued' waits for the measurement to be written.
		waitUntil(t, 10*time.Second, "the parked tick row to be stamped", func() bool {
			return schedGap157TickStatus(t, db, tickID) != "queued"
		})
		measured, _, _ = schedGap157Stamps(t, db, tickID)
		measuredTick = tickID
		t.Logf("SCHED-GAP-157 slot-wait attempt %d: tick %s waited %d ms (clock advanced %d ms while it was parked)",
			attempt, tickID, measured, (advanceBefore + advanceAfter).Milliseconds())
		if measured > 0 {
			break
		}

		// Scheduling artifact: the goroutine had not read waitStart before the
		// advance, so it measured a zero-length wait. Drain the pool's
		// bookkeeping, re-take the single slot (so the next attempt's tick must
		// wait again), and retry with fresh fixtures.
		l.slotPool.ReleaseAll()
		if !l.slotPool.Acquire(context.Background(), holder) {
			t.Fatal("Acquire for the slot holder failed on retry")
		}
	}

	if measured <= 0 {
		t.Fatalf("tick %s recorded slot_wait_ms = %d after 3 parked spawns — a tick that waited for the pool's only free slot must record a NON-ZERO wait on the injected clock; 0 here means the pool does not measure its slot wait", measuredTick, measured)
	}
	if maxWait := (advanceBefore + advanceAfter).Milliseconds(); measured > maxWait {
		t.Errorf("tick %s recorded slot_wait_ms = %d, exceeding the %d ms the injected clock advanced while it was parked — the measured wait must come from the loop's clock seam", measuredTick, measured, maxWait)
	}

	if slotWait, reason, nudge := schedGap157Stamps(t, db, measuredTick); slotWait != measured || reason != AdmissionReasonOK || nudge != "" {
		t.Errorf("parked tick %s re-read = slot_wait_ms %d admit_reason %q nudge_source %q, want %d/%q/\"\" (a packer-path spawn carries no nudge source)",
			measuredTick, slotWait, reason, nudge, measured, AdmissionReasonOK)
	}

	// Let every parked spawn finish so no goroutine outlives the test.
	gw.releaseAll()
	waitForSettled(t, db, 30*time.Second)
}

// TestSCHEDGAP157_SlotPoolStampsZeroForAFreeSlot is the CONTRACT CONTROL for the
// measured wait above: a tick whose slot is free when it reaches the pool
// records 0 — "never waited" — with the ok admission decision and no nudge
// source. Without this, "the value is measured" would be satisfied by a pool
// that stamps any constant at all.
func TestSCHEDGAP157_SlotPoolStampsZeroForAFreeSlot(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 1)
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)
	gw := newHeldResumeGateway(t)
	gw.wire(l)

	const proj = "157-free-slot"
	admitInsertProject(t, db, admitProjectSpec{Name: proj, CooldownS: 21600})
	tickID := database.NextTickID(clock.WithClock(context.Background(), sim), proj)
	queuedTickRow(t, db, tickID, proj)
	l.slotPool.SpawnEnqueued(PackedProject{Name: proj}, tickID, sim.Now(), true, db)

	waitUntil(t, 10*time.Second, "the free-slot tick to be stamped", func() bool {
		return schedGap157TickStatus(t, db, tickID) != "queued"
	})
	slotWait, reason, nudge := schedGap157Stamps(t, db, tickID)
	if slotWait != 0 {
		t.Errorf("tick %s recorded slot_wait_ms = %d with a free slot, want 0 — 0 is the \"never waited\" value and must not be inflated by the measurement path", tickID, slotWait)
	}
	if reason != AdmissionReasonOK {
		t.Errorf("tick %s admit_reason = %q, want %q", tickID, reason, AdmissionReasonOK)
	}
	if nudge != "" {
		t.Errorf("tick %s nudge_source = %q, want \"\" — a packer-path spawn has no nudge source", tickID, nudge)
	}

	gw.releaseAll()
	waitForSettled(t, db, 30*time.Second)
}

// ── (b) DEFERRALS ────────────────────────────────────────────────────────

// TestSCHEDGAP157_DeferralRoundTripAndValidation is acceptance (b) at the
// writer/reader level: (project, reason, pass_id, detail) round-trips through
// RecordDeferral + ListDeferrals, listing is newest-first with an exact project
// filter and offset pagination, and the validated inputs (empty project, empty
// reason) are refused WITHOUT writing a row.
func TestSCHEDGAP157_DeferralRoundTripAndValidation(t *testing.T) {
	db := newTestDB(t)
	// created_at is written from the CONTEXT clock (SCHED-GAP-169), so pinning a
	// fixed instant makes the row's timestamp exact instead of "recently".
	at := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	ctx := clock.WithClock(context.Background(), clock.NewFixed(at))

	schedGap157CountDeferrals := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM deferrals`).Scan(&n); err != nil {
			t.Fatalf("count deferrals: %v", err)
		}
		return n
	}

	// Validation: rejected inputs must not reach the table.
	if err := database.RecordDeferral(ctx, db, "", AdmissionReasonCooldown, 1, ""); err == nil {
		t.Error("RecordDeferral with an empty project name returned nil, want an error")
	}
	if err := database.RecordDeferral(ctx, db, "157-defer-a", "", 1, ""); err == nil {
		t.Error("RecordDeferral with an empty reason returned nil, want an error")
	}
	if got := schedGap157CountDeferrals(); got != 0 {
		t.Fatalf("deferrals rows after two refused writes = %d, want 0 — a rejected input must not write a partial row", got)
	}

	// Three accepted rows: two passes for one project, one for another.
	type seed struct {
		project, reason string
		passID          int64
		detail          string
	}
	seeds := []seed{
		{"157-defer-a", AdmissionReasonCooldown, 11, "pass_id=11 eligible=2 admitted=0 deferred=2 ns=-"},
		{"157-defer-b", AdmissionReasonCap, 11, "pass_id=11 eligible=2 admitted=0 deferred=2 ns=capns"},
		{"157-defer-a", AdmissionReasonLoadGate, 12, "pass_id=12 eligible=2 admitted=0 deferred=2 ns=-"},
	}
	for _, s := range seeds {
		if err := database.RecordDeferral(ctx, db, s.project, s.reason, s.passID, s.detail); err != nil {
			t.Fatalf("RecordDeferral(%s, %s, %d): %v", s.project, s.reason, s.passID, err)
		}
	}

	all, err := database.ListDeferrals(ctx, db, "", 0, 0)
	if err != nil {
		t.Fatalf("ListDeferrals(all): %v", err)
	}
	if len(all) != len(seeds) {
		t.Fatalf("ListDeferrals(all) returned %d rows, want %d", len(all), len(seeds))
	}
	// Newest first: the last insert must come back first.
	for i, s := range []seed{seeds[2], seeds[1], seeds[0]} {
		got := all[i]
		if got.ProjectName != s.project || got.Reason != s.reason || got.PassID != s.passID || got.Detail != s.detail {
			t.Errorf("row %d (newest-first) = {%s %s pass=%d %q}, want {%s %s pass=%d %q}",
				i, got.ProjectName, got.Reason, got.PassID, got.Detail, s.project, s.reason, s.passID, s.detail)
		}
		if got.ID <= 0 {
			t.Errorf("row %d has id=%d, want a positive AUTOINCREMENT id", i, got.ID)
		}
		if got.CreatedAt != at.Format(time.RFC3339) {
			t.Errorf("row %d created_at = %q, want the context clock instant %q", i, got.CreatedAt, at.Format(time.RFC3339))
		}
	}
	if all[0].ID <= all[1].ID || all[1].ID <= all[2].ID {
		t.Errorf("ids are not descending (%d,%d,%d) — ListDeferrals must be newest-first", all[0].ID, all[1].ID, all[2].ID)
	}

	// Project filter: exactly the two rows of 157-defer-a, in pass order (newest first).
	filtered, err := database.ListDeferrals(ctx, db, "157-defer-a", 0, 0)
	if err != nil {
		t.Fatalf("ListDeferrals(157-defer-a): %v", err)
	}
	if len(filtered) != 2 {
		t.Fatalf("ListDeferrals(157-defer-a) returned %d rows, want 2", len(filtered))
	}
	if filtered[0].PassID != 12 || filtered[1].PassID != 11 {
		t.Errorf("filtered pass ids = (%d,%d), want (12,11) — the per-project view must be newest-first", filtered[0].PassID, filtered[1].PassID)
	}
	if filtered[0].Reason != AdmissionReasonLoadGate || filtered[1].Reason != AdmissionReasonCooldown {
		t.Errorf("filtered reasons = (%q,%q), want (%q,%q)", filtered[0].Reason, filtered[1].Reason, AdmissionReasonLoadGate, AdmissionReasonCooldown)
	}
	if other, err := database.ListDeferrals(ctx, db, "157-defer-none", 0, 0); err != nil || len(other) != 0 {
		t.Errorf("ListDeferrals for an unknown project = %d rows (err %v), want 0/nil", len(other), err)
	}

	// Pagination: skip the newest row, take one.
	paged, err := database.ListDeferrals(ctx, db, "", 1, 1)
	if err != nil {
		t.Fatalf("ListDeferrals(limit=1, offset=1): %v", err)
	}
	if len(paged) != 1 || paged[0].PassID != 11 || paged[0].ProjectName != "157-defer-b" {
		t.Errorf("paged result = %+v, want the second-newest row (157-defer-b pass 11)", paged)
	}
}

// TestSCHEDGAP157_EvaluationPassPersistsDeferralRows is acceptance (b) at the
// ADMISSION-PASS level: a candidate the real evaluation pass passes over gets a
// deferrals row whose reason is the same reason the grep-stable ADMIT line
// carries — i.e. "why was this lane skipped in window Y" is answerable from the
// table, not only from the log.
func TestSCHEDGAP157_EvaluationPassPersistsDeferralRows(t *testing.T) {
	now := fixedEvalNow()
	db := newTestDB(t)
	ctx := context.Background()

	// cooldown 3600s, last completed 600s before the pinned instant: the pass
	// must classify this candidate as a cooldown deferral.
	admitInsertProject(t, db, admitProjectSpec{
		Name: "157-def-cool", CooldownS: 3600, Last: now.Add(-600 * time.Second),
	})

	l := admitNewLoop(t, db, now) // sim mode + pinned clock (admission_decision_test.go)
	cap := admitCaptureLog(t)
	l.evaluate()

	rows, err := database.ListDeferrals(ctx, db, "157-def-cool", 0, 0)
	if err != nil {
		t.Fatalf("ListDeferrals: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("deferral rows for 157-def-cool = %d, want exactly 1 (one row per passed-over candidate per pass): %+v", len(rows), rows)
	}
	row := rows[0]

	lines := cap.projectLines("157-def-cool")
	if len(lines) != 1 {
		t.Fatalf("ADMIT lines for 157-def-cool = %d, want exactly 1", len(lines))
	}
	if got := admitField(lines[0], "reason"); row.Reason != got {
		t.Errorf("deferral row reason = %q, ADMIT line reason = %q — the persisted row and the log line must agree", row.Reason, got)
	}
	if row.Reason != AdmissionReasonCooldown {
		t.Errorf("deferral row reason = %q, want %q (a 3000s-remaining cooldown)", row.Reason, AdmissionReasonCooldown)
	}
	if row.PassID <= 0 {
		t.Errorf("deferral row pass_id = %d, want a positive pass id", row.PassID)
	}
	if got := admitInt(t, lines[0], "pass_id"); int64(got) != row.PassID {
		t.Errorf("deferral row pass_id = %d, ADMIT line pass_id = %d — the row must carry the pass it belongs to", row.PassID, got)
	}
	if row.Detail == "" {
		t.Error("deferral row detail is empty — the row must be self-contained (the pass header is copied into it)")
	}
}

// TestSCHEDGAP157_StarvationQuerySelectsOnlyStarvedLanes is the acceptance
// STARVATION QUERY (acceptance (b)): the single query over `ticks` documented
// above returns exactly the lanes that waited LONGER than
// StarvationThreshold (5m = 300000 ms) and nothing else — not lanes under the
// threshold, not a lane that waited exactly the threshold, and not legacy rows
// whose slot_wait_ms is the default 0.
func TestSCHEDGAP157_StarvationQuerySelectsOnlyStarvedLanes(t *testing.T) {
	db := newTestDB(t)
	thresholdMs := StarvationThreshold.Milliseconds()
	if thresholdMs != 300_000 {
		t.Fatalf("StarvationThreshold = %v (%d ms), want 5m/300000 ms — the acceptance query's bound", StarvationThreshold, thresholdMs)
	}

	seeds := []struct {
		project, tickID string
		waitMs          int64
	}{
		// Starved: past the patience the pool drops work at.
		{"157-star-a", "157-star-a-t1", thresholdMs + 1},
		{"157-star-b", "157-star-b-t1", 900_000}, // 15m
		{"157-star-b", "157-star-b-t2", 600_000}, // a second, shorter-but-still-starved tick
		// Not starved: exactly at the threshold (the predicate is strict >),
		// just under it, and a lane that never waited at all.
		{"157-at-threshold", "157-at-threshold-t1", thresholdMs},
		{"157-under", "157-under-t1", thresholdMs - 1},
		{"157-never", "157-never-t1", 0},
	}
	for _, s := range seeds {
		// schedGap157SeedWait creates the project row at most once per project.
		schedGap157SeedWait(t, db, s.project, s.tickID, s.waitMs)
	}

	got := schedGap157Starvation(t, db, thresholdMs)
	want := []schedGap157StarvedLane{
		{"157-star-b", 2, 900_000},
		{"157-star-a", 1, thresholdMs + 1},
	}
	if len(got) != len(want) {
		t.Fatalf("starvation query returned %d lanes %+v, want exactly %d %+v — only lanes that waited PAST the threshold count", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("starved lane %d = %+v, want %+v (most-starved first)", i, got[i], want[i])
		}
	}
	// The exclusion half, asserted by name so a failure names the lane that was
	// wrongly reported as starved.
	for _, name := range []string{"157-at-threshold", "157-under", "157-never"} {
		for _, l := range got {
			if l.project == name {
				t.Errorf("lane %s reported as starved (max wait %d ms) — it never waited past the %d ms threshold", name, l.maxWait, thresholdMs)
			}
		}
	}
}

// ── (c) LEGACY ROWS ──────────────────────────────────────────────────────

// TestSCHEDGAP157_LegacyRowsReadAsDefaultsAfterMigration is acceptance (c): a
// tick row that existed BEFORE migration v35 reads back slot_wait_ms=0,
// admit_reason="" and nudge_source="" after the upgrade — the SQL column
// defaults, never a panic, never a fabricated value.
//
// The pre-v35 database is simulated IN PLACE rather than by hand-writing a
// scratch schema: the three v35 columns are dropped and the v35 row is removed
// from `migrations` (plus the deferrals table), which is exactly the state a
// v34 database on disk is in. database.Migrate — the function the daemon runs
// at boot — then re-applies v35 over a table that already holds rows, so this
// pins the real upgrade path (ALTER TABLE ... NOT NULL DEFAULT backfill).
func TestSCHEDGAP157_LegacyRowsReadAsDefaultsAfterMigration(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const legacyProj, legacyID = "157-legacy", "157-legacy-2026-09-01-00-00-00"
	admitInsertProject(t, db, admitProjectSpec{Name: legacyProj, CooldownS: 21600})
	queuedTickRow(t, db, legacyID, legacyProj)
	// A pre-existing row with real data in the columns that already existed:
	// the migration must leave every one of them alone.
	if _, err := db.Exec(`UPDATE ticks SET status='completed', outcome='committed', commits=7,
		files_changed=3, tokens_in=1234, cost_usd=1.25, error=NULL WHERE id = ?`, legacyID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	before := struct {
		status, outcome        string
		commits, files, tokens int64
		cost                   float64
	}{}
	if err := db.QueryRow(`SELECT status, outcome, commits, files_changed, tokens_in, cost_usd FROM ticks WHERE id = ?`, legacyID).
		Scan(&before.status, &before.outcome, &before.commits, &before.files, &before.tokens, &before.cost); err != nil {
		t.Fatalf("read seeded legacy row: %v", err)
	}

	// ── roll the schema back to its pre-v35 shape ──
	for _, col := range []string{"slot_wait_ms", "admit_reason", "nudge_source"} {
		if _, err := db.Exec(`ALTER TABLE ticks DROP COLUMN ` + col); err != nil {
			t.Fatalf("simulate pre-v35 schema (drop %s): %v", col, err)
		}
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS deferrals`); err != nil {
		t.Fatalf("simulate pre-v35 schema (drop deferrals): %v", err)
	}
	if _, err := db.Exec(`DELETE FROM migrations WHERE version = 35`); err != nil {
		t.Fatalf("forget migration v35: %v", err)
	}
	// PREMISE: the legacy database really is missing the column. Without this,
	// the read-back below would be satisfied by a schema that never changed.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ticks') WHERE name = 'slot_wait_ms'`).Scan(&n); err != nil {
		t.Fatalf("inspect ticks schema: %v", err)
	}
	if n != 0 {
		t.Fatalf("premise: ticks.slot_wait_ms still exists after the simulated rollback (count=%d) — the fixture did not reproduce a pre-v35 database", n)
	}

	// ── the boot-time migration path ──
	if err := database.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate over a pre-v35 database: %v", err)
	}

	// The upgrade really ran: columns back, v35 recorded, deferrals table back.
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ticks') WHERE name IN ('slot_wait_ms','admit_reason','nudge_source')`).Scan(&n); err != nil {
		t.Fatalf("inspect ticks schema after migration: %v", err)
	}
	if n != 3 {
		t.Fatalf("ticks has %d of the 3 SCHED-GAP-157 columns after Migrate, want 3", n)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM migrations WHERE version = 35`).Scan(&n); err != nil {
		t.Fatalf("read migrations row for v35: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrations rows for v35 = %d after Migrate, want 1", n)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='deferrals'`).Scan(&n); err != nil {
		t.Fatalf("inspect deferrals table: %v", err)
	}
	if n != 1 {
		t.Fatalf("deferrals table missing after Migrate (count=%d)", n)
	}

	// ── the legacy row ──
	slotWait, admitReason, nudgeSource := schedGap157Stamps(t, db, legacyID)
	if slotWait != 0 {
		t.Errorf("legacy tick slot_wait_ms = %d, want 0 — a pre-v35 row never measured a wait; fabricating a value would be a lie about a tick the pool never recorded", slotWait)
	}
	if admitReason != "" {
		t.Errorf("legacy tick admit_reason = %q, want \"\" — the pre-v35 row carries no admission decision", admitReason)
	}
	if nudgeSource != "" {
		t.Errorf("legacy tick nudge_source = %q, want \"\" — an empty source is the packer/default value, never back-filled with a guess", nudgeSource)
	}

	// Never a panic, never a fabricated value: the model read path agrees.
	tk, err := database.GetTick(ctx, db, legacyID)
	if err != nil {
		t.Fatalf("GetTick on a legacy row: %v", err)
	}
	if tk.SlotWaitMs != 0 || tk.AdmitReason != "" || tk.NudgeSource != "" {
		t.Errorf("GetTick on a legacy row = slot_wait_ms %d admit_reason %q nudge_source %q, want 0/\"\"/\"\"", tk.SlotWaitMs, tk.AdmitReason, tk.NudgeSource)
	}

	// The migration must not clobber what the row already held.
	var after struct {
		status, outcome        string
		commits, files, tokens int64
		cost                   float64
	}
	if err := db.QueryRow(`SELECT status, outcome, commits, files_changed, tokens_in, cost_usd FROM ticks WHERE id = ?`, legacyID).
		Scan(&after.status, &after.outcome, &after.commits, &after.files, &after.tokens, &after.cost); err != nil {
		t.Fatalf("re-read legacy row: %v", err)
	}
	if after != before {
		t.Errorf("legacy row changed across the v35 migration: %+v → %+v", before, after)
	}

	// And a legacy row is never misclassified as starved by the acceptance
	// query (its 0 is not > the 300000 ms threshold).
	for _, l := range schedGap157Starvation(t, db, StarvationThreshold.Milliseconds()) {
		if l.project == legacyProj {
			t.Errorf("legacy lane %s reported as starved (max wait %d ms) — a row that never measured a wait must not appear in a starvation report", legacyProj, l.maxWait)
		}
	}
}

// ── (d) NUDGE SOURCES ────────────────────────────────────────────────────

// TestSCHEDGAP157_NudgeSourceStampedDistinctly is acceptance (d) at the stamp
// level: the three non-packer entry points stamp three DISTINCT values, the
// literals are the DB contract the constants promise, a packer tick (empty
// source) does NOT overwrite an existing nudge_source, and a non-empty source
// DOES (so "the column is never written" fails here).
func TestSCHEDGAP157_NudgeSourceStampedDistinctly(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// The literals are the contract: these strings land in the DB and are read
	// back by operators, so a redefined constant must fail here.
	for _, c := range []struct{ name, got, want string }{
		{"NudgeSourceStartup", NudgeSourceStartup, "startup"},
		{"NudgeSourceManual", NudgeSourceManual, "manual"},
		{"NudgeSourceBoardWake", NudgeSourceBoardWake, "board_wake"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	sources := []string{NudgeSourceStartup, NudgeSourceManual, NudgeSourceBoardWake}
	seen := map[string]string{}
	for i, src := range sources {
		proj := fmt.Sprintf("157-nudge-%d", i)
		tickID := proj + "-t1"
		admitInsertProject(t, db, admitProjectSpec{Name: proj, CooldownS: 21600})
		queuedTickRow(t, db, tickID, proj)
		if err := database.RecordTickAdmission(ctx, db, tickID, 7*time.Second, AdmissionReasonOK, src); err != nil {
			t.Fatalf("RecordTickAdmission(%s, nudge=%s): %v", tickID, src, err)
		}
		wait, reason, nudge := schedGap157Stamps(t, db, tickID)
		if wait != 7_000 || reason != AdmissionReasonOK || nudge != src {
			t.Errorf("tick %s = slot_wait_ms %d admit_reason %q nudge_source %q, want 7000/%q/%q", tickID, wait, reason, nudge, AdmissionReasonOK, src)
		}
		if prev, dup := seen[nudge]; dup {
			t.Errorf("nudge_source %q is shared by %s and %s — the three entry points must be distinguishable", nudge, prev, proj)
		}
		seen[nudge] = proj
	}

	// The packer tick: nudgeSource == "" must leave the enqueue-path stamp alone
	// (the packer path IS the default; re-stamping it "" would erase the reason
	// the row exists outside the packer).
	const packerProj, packerTick = "157-packer", "157-packer-t1"
	admitInsertProject(t, db, admitProjectSpec{Name: packerProj, CooldownS: 21600})
	queuedTickRow(t, db, packerTick, packerProj)
	if err := database.RecordTickAdmission(ctx, db, packerTick, 0, AdmissionReasonOK, NudgeSourceStartup); err != nil {
		t.Fatalf("seed enqueue-path nudge stamp: %v", err)
	}
	if err := database.RecordTickAdmission(ctx, db, packerTick, 12*time.Second, AdmissionReasonOK, ""); err != nil {
		t.Fatalf("packer-path stamp: %v", err)
	}
	wait, reason, nudge := schedGap157Stamps(t, db, packerTick)
	if nudge != NudgeSourceStartup {
		t.Errorf("packer tick nudge_source = %q, want %q — a tick with no nudge source must not overwrite the stamp its enqueue path wrote", nudge, NudgeSourceStartup)
	}
	if wait != 12_000 || reason != AdmissionReasonOK {
		t.Errorf("packer tick = slot_wait_ms %d admit_reason %q, want 12000/%q — the empty value suppresses ONLY the nudge_source column", wait, reason, AdmissionReasonOK)
	}

	// Positive control for the column-write contract: a NON-empty source does
	// overwrite, so a mutation that drops nudge_source from the UPDATE fails.
	if err := database.RecordTickAdmission(ctx, db, packerTick, 12*time.Second, AdmissionReasonOK, NudgeSourceBoardWake); err != nil {
		t.Fatalf("overwrite nudge stamp: %v", err)
	}
	if _, _, nudge := schedGap157Stamps(t, db, packerTick); nudge != NudgeSourceBoardWake {
		t.Errorf("nudge_source after a non-empty stamp = %q, want %q — a non-empty source must be written", nudge, NudgeSourceBoardWake)
	}

	// Best-effort writer contract: an unknown id is reported, not silently
	// swallowed (the caller logs it; the tick's scheduling outcome is unchanged).
	err := database.RecordTickAdmission(ctx, db, "157-no-such-tick", time.Second, AdmissionReasonOK, NudgeSourceManual)
	if !errors.Is(err, database.ErrTickNotFound) {
		t.Errorf("RecordTickAdmission for an unknown id = %v, want database.ErrTickNotFound", err)
	}
}

// TestSCHEDGAP157_SlotPoolStampsAdmitReasonForNudgeTicks is acceptance (d)
// through the REAL pool: a nudge spawn carries its entry point (`nudge_source`)
// and the admission decision that let it in (`admit_reason` = "resume:<source>"),
// while a packer spawn records "ok" and leaves an existing nudge_source
// untouched. Every slot is free here, so no wait is involved — the stamps are
// read after the row leaves `queued`, which the pool only does after the stamp
// (slot_pool.go: stampTickAdmission precedes Enqueue/StartRunning).
func TestSCHEDGAP157_SlotPoolStampsAdmitReasonForNudgeTicks(t *testing.T) {
	db := newTestDB(t)

	l := NewLoop(db, time.Minute, time.Hour, 10, 100, 8) // one slot per spawn below
	l.noDeliver = true
	sim := clock.NewManualSimClock(time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC))
	t.Cleanup(sim.Close)
	l.SetClock(sim)
	gw := newHeldResumeGateway(t)
	gw.wire(l)

	// spawnNudged hands the pool a queued row (the shape every non-packer entry
	// point uses: the row is enqueued first, then the tick is handed to the pool)
	// with the caller's nudge stamp set — via Loop.SetNudgeSource, or via the
	// board watcher's own hook for the board_wake case.
	spawnNudged := func(t *testing.T, proj string, stamp func()) string {
		t.Helper()
		qs := fmt.Sprintf("157-nudge-%s", proj)
		capTestNamespace(t, db, qs, 4, "cooldown")
		admitInsertProject(t, db, admitProjectSpec{Name: proj, NS: qs, CooldownS: 21600})
		tickID := database.NextTickID(clock.WithClock(context.Background(), sim), proj)
		queuedTickRow(t, db, tickID, proj)
		stamp()
		l.slotPool.SpawnEnqueued(PackedProject{Name: proj, NamespaceID: qs}, tickID, sim.Now(), true, db)
		return tickID
	}

	// manual + startup: the operator/API and the boot-resume entry points.
	for i, src := range []string{NudgeSourceManual, NudgeSourceStartup} {
		proj := fmt.Sprintf("157-pool-nudge-%d", i)
		tickID := spawnNudged(t, proj, func() { l.SetNudgeSource(src) })
		waitUntil(t, 10*time.Second, "the "+src+" nudge tick to be stamped", func() bool {
			return schedGap157TickStatus(t, db, tickID) != "queued"
		})
		wait, reason, nudge := schedGap157Stamps(t, db, tickID)
		if nudge != src {
			t.Errorf("%s nudge tick nudge_source = %q, want %q — the pool must consume (and record) the caller's stamp", src, nudge, src)
		}
		if want := "resume:" + src; reason != want {
			t.Errorf("%s nudge tick admit_reason = %q, want %q (SlotPool.spawn's nudge contract)", src, reason, want)
		}
		if wait != 0 {
			t.Errorf("%s nudge tick slot_wait_ms = %d, want 0 (its slot was free)", src, wait)
		}
	}

	// board_wake: the watcher's own hook (installed by NewLoop) must reach the
	// row through the same path.
	boardTick := spawnNudged(t, "157-pool-nudge-bw", func() {
		if f, ok := boardWakeNudgeSource.Load().(func()); ok {
			f()
		} else {
			t.Fatalf("boardWakeNudgeSource holds no hook after NewLoop — the board-wake nudge source cannot be stamped")
		}
	})
	waitUntil(t, 10*time.Second, "the board_wake nudge tick to be stamped", func() bool {
		return schedGap157TickStatus(t, db, boardTick) != "queued"
	})
	if _, reason, nudge := schedGap157Stamps(t, db, boardTick); nudge != NudgeSourceBoardWake || reason != "resume:"+NudgeSourceBoardWake {
		t.Errorf("board_wake tick = admit_reason %q nudge_source %q, want %q/%q", reason, nudge, "resume:"+NudgeSourceBoardWake, NudgeSourceBoardWake)
	}

	// The packer tick (no stamp at all) must record "ok" and leave the
	// enqueue-path nudge_source — seeded here the way the boot resume scan would
	// have — exactly as it was.
	const packerProj = "157-pool-packer"
	capTestNamespace(t, db, "157-nudge-packer", 4, "cooldown")
	admitInsertProject(t, db, admitProjectSpec{Name: packerProj, NS: "157-nudge-packer", CooldownS: 21600})
	packerTick := database.NextTickID(clock.WithClock(context.Background(), sim), packerProj)
	queuedTickRow(t, db, packerTick, packerProj)
	if err := database.RecordTickAdmission(context.Background(), db, packerTick, 0, "", NudgeSourceStartup); err != nil {
		t.Fatalf("seed enqueue-path nudge stamp on the packer row: %v", err)
	}
	l.slotPool.SpawnEnqueued(PackedProject{Name: packerProj, NamespaceID: "157-nudge-packer"}, packerTick, sim.Now(), true, db)
	waitUntil(t, 10*time.Second, "the packer tick to be stamped", func() bool {
		return schedGap157TickStatus(t, db, packerTick) != "queued"
	})
	wait, reason, nudge := schedGap157Stamps(t, db, packerTick)
	if reason != AdmissionReasonOK {
		t.Errorf("packer tick admit_reason = %q, want %q", reason, AdmissionReasonOK)
	}
	if nudge != NudgeSourceStartup {
		t.Errorf("packer tick nudge_source = %q, want the untouched %q — a spawn with no nudge stamp must not clear the row's source", nudge, NudgeSourceStartup)
	}
	if wait != 0 {
		t.Errorf("packer tick slot_wait_ms = %d, want 0 (its slot was free)", wait)
	}

	gw.releaseAll()
	waitForSettled(t, db, 30*time.Second)
}
