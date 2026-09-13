package scheduler

import (
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-107-VER verification suite (scheduler layer). bump_test.go
// covers the hook pair end-to-end; this file pins the parts it never
// asserts: the bumpTickCompleted RETURN-VALUE contract (Phase A signal),
// Phase A completeness BEFORE Phase B runs, the hard-cap force-revert on an
// UNFLAGGED tick, the packer bump-override in the remaining selection path
// (multi-pool packFlat), and the countEligibleProjects eligibility mirror.

// TestVerify_TwoPhaseRevert_FinalTickReturnsTrueAndRestores pins the caller
// contract of bumpTickCompleted: false on interior ticks (Phase A must NOT
// run), true on the final tick with Phase A ALREADY APPLIED — the restored
// baseline is observable in the DB before the caller's adaptiveCooldown
// (Phase B) has run at all.
func TestVerify_TwoPhaseRevert_FinalTickReturnsTrueAndRestores(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "two-phase", 43200, 21600, 604800, 2)
	if _, err := database.BumpProject(t.Context(), db, "two-phase", 3, 7200, "verify"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}

	// Interior ticks: return false, bump stays active, countdown decrements
	// exactly once per completed bump tick.
	for i := 1; i <= 2; i++ {
		id := "tp-interior-" + string(rune('0'+i))
		insertBumpTickRow(t, db, "two-phase", id, 1)
		oc := noProgressOutcome("two-phase")
		oc.TickID = id
		if bumpTickCompleted(db, "two-phase", "/tmp/work/two-phase", oc) {
			t.Fatalf("interior tick %d: bumpTickCompleted = true, want false (no Phase A yet)", i)
		}
		s := readBumpState(t, db, "two-phase")
		if s.bumpActive != 1 || s.bumpRemaining != 3-i {
			t.Fatalf("interior tick %d: active=%d remaining=%d, want 1/%d", i, s.bumpActive, s.bumpRemaining, 3-i)
		}
	}

	// Final tick: returns true AND Phase A is complete at return time — the
	// restored cooldown (43200) is in the DB BEFORE any Phase B call. This
	// is what guarantees Phase B evaluates the restored baseline, not the
	// bump cooldown.
	final := TickOutcome{Project: "two-phase", Status: TickCompleted, Commits: 2}
	final.TickID = "tp-final"
	insertBumpTickRow(t, db, "two-phase", "tp-final", 1)
	if !bumpTickCompleted(db, "two-phase", "/tmp/work/two-phase", final) {
		t.Fatal("final bump tick: bumpTickCompleted = false, want true (Phase A performed)")
	}
	s := readBumpState(t, db, "two-phase")
	if s.bumpActive != 0 {
		t.Fatal("bump still active after final bump tick (Phase A not applied)")
	}
	if s.cooldown != 43200 || s.streak != 2 || s.floor != 21600 || s.ceiling != 604800 {
		t.Fatalf("Phase A restore wrong pre-PhaseB: cd=%d streak=%d floor=%d ceiling=%d, want 43200/2/21600/604800",
			s.cooldown, s.streak, s.floor, s.ceiling)
	}

	// Phase B is the CALLER's job: running adaptiveCooldown now over the
	// real-work outcome lands at the floor (worked example 1: 43200→21600).
	if !adaptiveCooldown(db, "two-phase", "/tmp/work/two-phase", final) {
		t.Fatal("adaptiveCooldown returned false — Phase B did not run (adaptive disabled?)")
	}
	s = readBumpState(t, db, "two-phase")
	if s.cooldown != 21600 {
		t.Fatalf("post-PhaseB cooldown = %d, want 21600 (real work → floor)", s.cooldown)
	}
}

// TestVerify_TwoPhaseRevert_IdleResumesRestoredStreak pins the idle side of
// Phase B: the restored streak resumes exactly where it was — ONE idle
// evaluation post-revert, not N. Pre-bump streak 9 (threshold 10): one idle
// eval moves 9→10 → escalate 43200→86400; a bump that leaked 5 idle evals
// would land far higher (43200×2^5).
func TestVerify_TwoPhaseRevert_IdleResumesRestoredStreak(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "idle-resume", 43200, 21600, 604800, 9)
	if _, err := database.BumpProject(t.Context(), db, "idle-resume", 4, 7200, "verify idle"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	for i := 0; i < 4; i++ {
		insertBumpTickRow(t, db, "idle-resume", "ir-"+string(rune('0'+i)), 1)
		bumpTick(t, db, "idle-resume", "ir-"+string(rune('0'+i)), noProgressOutcome("idle-resume"))
	}
	s := readBumpState(t, db, "idle-resume")
	if s.bumpActive != 0 {
		t.Fatal("bump not reverted after 4 idle bump ticks")
	}
	if s.streak != 10 {
		t.Fatalf("streak = %d, want 10 (restored 9 + exactly ONE post-revert idle eval)", s.streak)
	}
	if s.cooldown != 86400 {
		t.Fatalf("cooldown = %d, want 86400 (single 2x escalation from restored 43200)", s.cooldown)
	}
}

// TestVerify_HardCap_UnflaggedTickForceReverts pins the hard-cap branch
// bump_test.go never touches: a tick spawned BEFORE bump activation (bump=0)
// does not consume the countdown while fresh, but STILL force-reverts once
// the 12h cap passes — a stuck bump cannot outlive its window.
func TestVerify_HardCap_UnflaggedTickForceReverts(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "cap-unflagged", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "cap-unflagged", 5, 7200, "cap"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}

	// Fresh unflagged tick: no consumption, no revert (returns false).
	insertBumpTickRow(t, db, "cap-unflagged", "cu-fresh", 0)
	fresh := noProgressOutcome("cap-unflagged")
	fresh.TickID = "cu-fresh"
	if bumpTickCompleted(db, "cap-unflagged", "/tmp/work/cap-unflagged", fresh) {
		t.Fatal("fresh unflagged tick: bumpTickCompleted = true, want false (no consumption)")
	}
	s := readBumpState(t, db, "cap-unflagged")
	if s.bumpActive != 1 || s.bumpRemaining != 5 {
		t.Fatalf("fresh unflagged tick consumed the bump: active=%d remaining=%d", s.bumpActive, s.bumpRemaining)
	}

	// Age the bump past the 12h hard cap; the next completion — still
	// unflagged — force-reverts with ticks remaining.
	old := time.Now().UTC().Add(-13 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET bump_started_at = ? WHERE name = 'cap-unflagged'`, old); err != nil {
		t.Fatalf("age bump: %v", err)
	}
	insertBumpTickRow(t, db, "cap-unflagged", "cu-old", 0)
	oc := noProgressOutcome("cap-unflagged")
	oc.TickID = "cu-old"
	if !bumpTickCompleted(db, "cap-unflagged", "/tmp/work/cap-unflagged", oc) {
		t.Fatal("expired unflagged tick: bumpTickCompleted = false, want true (hard-cap force-revert)")
	}
	s = readBumpState(t, db, "cap-unflagged")
	if s.bumpActive != 0 {
		t.Fatal("bump survived past the 12h hard cap via unflagged ticks")
	}
	if s.cooldown != 43200 {
		t.Fatalf("cooldown = %d after hard-cap revert, want 43200", s.cooldown)
	}
}

// TestVerify_AdaptiveStillRunsDuringBump pins the law the packer override
// exists for: adaptiveCooldown keeps running on bump ticks, so cooldown_s can
// escalate ABOVE bump_cooldown_s mid-bump — only the packer override keeps
// the bump governing selection (RED-check mutation 1 targets exactly this).
func TestVerify_AdaptiveStillRunsDuringBump(t *testing.T) {
	db := slowdownTestDB(t)
	// Streak already past the threshold (10): the first idle bump tick
	// escalates cooldown_s 7200 → 14400 while bump_active stays 1.
	bumpTestProject(t, db, "still-adaptive", 43200, 21600, 604800, 10)
	if _, err := database.BumpProject(t.Context(), db, "still-adaptive", 5, 7200, "adaptive-on"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	insertBumpTickRow(t, db, "still-adaptive", "sa-1", 1)
	bumpTick(t, db, "still-adaptive", "sa-1", noProgressOutcome("still-adaptive"))
	s := readBumpState(t, db, "still-adaptive")
	if s.bumpActive != 1 {
		t.Fatal("interior bump tick reverted the bump early")
	}
	if s.cooldown <= s.bumpCooldown {
		t.Fatalf("mid-bump cooldown = %d, want > bump cooldown %d (adaptive must keep running during a bump; the packer override is what keeps the bump governing selection)",
			s.cooldown, s.bumpCooldown)
	}
}

// TestVerify_PackerOverride_PackFlatPath covers the third selection path:
// multi-pool packFlat (no namespaces → Pack's flat fallback). The bump
// cooldown must override the STORED (adaptive-escalated) cooldown here too.
func TestVerify_PackerOverride_PackFlatPath(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "flat-bumped", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "flat-bumped", 5, 7200, "flat verify"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// Simulate mid-bump adaptive escalation: stored cooldown 86400 ≫ bump 7200.
	if _, err := db.Exec(`UPDATE projects SET cooldown_s = 86400 WHERE name = 'flat-bumped'`); err != nil {
		t.Fatalf("escalate cooldown: %v", err)
	}

	ctx := t.Context()
	projs, err := database.ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	calc := NewUrgencyCalculator(time.Minute, 24*time.Hour, 10)
	mp := NewMultiPoolPacker(100, 4, nil)
	mp.SetPendingCounter(NewPendingTaskCounter(0))

	// Last tick completed 3h ago: OUTSIDE the 7200s bump cooldown, INSIDE
	// the stored 86400s — selected only if the bump override is present.
	last := time.Now().UTC().Add(-3 * time.Hour)
	res := mp.Pack(projs, nil, calc, map[string]time.Time{"flat-bumped": last}, nil, time.Now())
	found := false
	for _, pp := range res.Projects {
		if pp.Name == "flat-bumped" {
			found = true
			if pp.Urgency < bumpBoostUrgency {
				t.Fatalf("flat-bumped urgency = %v, want >= bump tier %v", pp.Urgency, bumpBoostUrgency)
			}
		}
	}
	if !found {
		t.Fatal("bumped project not selected by packFlat despite bump cooldown elapsed (3h > 7200s) — override missing from the flat selection path")
	}

	// Control: last tick 1h ago (< 7200s bump cooldown) → NOT selected.
	last = time.Now().UTC().Add(-1 * time.Hour)
	res = mp.Pack(projs, nil, calc, map[string]time.Time{"flat-bumped": last}, nil, time.Now())
	for _, pp := range res.Projects {
		if pp.Name == "flat-bumped" {
			t.Fatal("bumped project selected by packFlat inside its bump cooldown window (1h < 7200s)")
		}
	}
}

// TestVerify_EligibleMirror_UsesBumpCooldown pins the countEligibleProjects
// mirror (GAP-050 parity): a bumped project whose STORED cooldown has not
// elapsed but whose BUMP cooldown has IS eligible — otherwise a healthy
// bump-driven select would alarm as EVAL-ZERO-SELECT.
func TestVerify_EligibleMirror_UsesBumpCooldown(t *testing.T) {
	db := slowdownTestDB(t)
	insertTestProject(t, db, "mirror-bumped", 86400, true, "")
	if _, err := database.BumpProject(t.Context(), db, "mirror-bumped", 5, 7200, "mirror"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// Simulate mid-bump adaptive escalation: the STORED cooldown (86400) is
	// now far above the bump cooldown (7200) — exactly the state the mirror
	// override exists for. Without it, eligibility would use 86400 and a
	// healthy bump select would alarm as EVAL-ZERO-SELECT.
	if _, err := db.Exec(`UPDATE projects SET cooldown_s = 86400 WHERE name = 'mirror-bumped'`); err != nil {
		t.Fatalf("escalate stored cooldown: %v", err)
	}
	// Last tick 3h ago → stored 86400s not elapsed, bump 7200s elapsed.
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = 'mirror-bumped'`,
		time.Now().UTC().Add(-3*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("set last_tick_completed: %v", err)
	}
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	if got := l.countEligibleProjects(time.Now(), map[string]bool{}); got != 1 {
		t.Fatalf("countEligibleProjects = %d, want 1 (bump cooldown 7200s elapsed overrides stored 86400s)", got)
	}

	// Control: 1h ago — inside the bump window too → not eligible.
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = 'mirror-bumped'`,
		time.Now().UTC().Add(-1*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("reset last_tick_completed: %v", err)
	}
	if got := l.countEligibleProjects(time.Now(), map[string]bool{}); got != 0 {
		t.Fatalf("countEligibleProjects = %d, want 0 (bump cooldown window 7200s not elapsed)", got)
	}
}
