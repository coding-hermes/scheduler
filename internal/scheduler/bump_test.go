package scheduler

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// bumpTestProject inserts an adaptive-enabled project with an explicit
// pre-bump policy so bump tests can verify the snapshot/restore exactly.
func bumpTestProject(t *testing.T, db *sql.DB, name string, cooldownS, floorS, ceilingS, streak int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s,
		 no_progress_threshold, no_progress_ticks)
		VALUES (?, ?, ?, 10, 5, ?, 1.0, 'deepseek-v4-pro', 'deepseek-foreman', 1,
		        datetime('now'), datetime('now'), 1, ?, ?, 10, ?)`,
		name, "https://github.com/example/"+name, "/tmp/work/"+name,
		cooldownS, floorS, ceilingS, streak)
	if err != nil {
		t.Fatalf("insert bump project %s: %v", name, err)
	}
}

// readBumpState reads the full bump-relevant column set for one project.
type bumpState struct {
	cooldown, floor, ceiling, streak int
	bumpActive, bumpRemaining        int
	bumpCooldown                     int
	bumpReason, bumpStarted          string
	savedCooldown, savedFloor        int
	savedCeiling, savedStreak        int
}

func readBumpState(t *testing.T, db *sql.DB, name string) bumpState {
	t.Helper()
	var s bumpState
	err := db.QueryRow(`SELECT cooldown_s, cooldown_floor_s, cooldown_ceiling_s,
	       no_progress_ticks, bump_active, bump_remaining_ticks, bump_cooldown_s,
	       bump_reason, bump_started_at,
	       bump_saved_cooldown_s, bump_saved_floor_s, bump_saved_ceiling_s,
	       bump_saved_no_progress_ticks
	FROM projects WHERE name = ?`, name).Scan(
		&s.cooldown, &s.floor, &s.ceiling, &s.streak,
		&s.bumpActive, &s.bumpRemaining, &s.bumpCooldown,
		&s.bumpReason, &s.bumpStarted,
		&s.savedCooldown, &s.savedFloor, &s.savedCeiling, &s.savedStreak)
	if err != nil {
		t.Fatalf("read bump state for %s: %v", name, err)
	}
	return s
}

// bumpTick simulates one completed bump tick through the real hook pair
// (bump accounting, then adaptive evaluation — exactly the slot-pool order).
func bumpTick(t *testing.T, db *sql.DB, name, tickID string, outcome TickOutcome) {
	t.Helper()
	outcome.TickID = tickID
	bumpTickCompleted(db, name, "/tmp/work/"+name, outcome)
	_ = adaptiveCooldown(db, name, "/tmp/work/"+name, outcome)
}

// insertBumpTickRow creates a terminal tick row with the given bump flag so
// bumpTickCompleted can read it back.
func insertBumpTickRow(t *testing.T, db *sql.DB, name, tickID string, bump int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ticks (id, project_name, status, bump, created_at)
		VALUES (?, ?, 'completed', ?, datetime('now'))`, tickID, name, bump)
	if err != nil {
		t.Fatalf("insert tick row %s: %v", tickID, err)
	}
}

// =============================================================================
// 1. Project-wide bump: snapshot + immediate cooldown effect.
// =============================================================================

func TestBump_ProjectWide(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "bumped-proj", 43200, 21600, 604800, 3)

	p, err := database.BumpProject(t.Context(), db, "bumped-proj", 5, 7200, "clear the backlog")
	if err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	if !p.BumpActive || p.BumpRemainingTicks != 5 || p.BumpCooldownS != 7200 {
		t.Fatalf("bump fields wrong: active=%v remaining=%d cooldown=%d", p.BumpActive, p.BumpRemainingTicks, p.BumpCooldownS)
	}
	if p.CooldownS != 7200 {
		t.Fatalf("cooldown_s = %d, want 7200 (immediate effect)", p.CooldownS)
	}
	s := readBumpState(t, db, "bumped-proj")
	if s.savedCooldown != 43200 || s.savedFloor != 21600 || s.savedCeiling != 604800 || s.savedStreak != 3 {
		t.Fatalf("saved snapshot wrong: cd=%d floor=%d ceiling=%d streak=%d", s.savedCooldown, s.savedFloor, s.savedCeiling, s.savedStreak)
	}
}

// =============================================================================
// 2. Auto-revert: 5 bump ticks consume the countdown; the 5th reverts.
// =============================================================================

func TestBump_AutoRevert(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "auto-revert", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "auto-revert", 5, 7200, "test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}

	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("auto-revert-tick-%d", i)
		insertBumpTickRow(t, db, "auto-revert", id, 1)
		bumpTick(t, db, "auto-revert", id, noProgressOutcome("auto-revert"))
		s := readBumpState(t, db, "auto-revert")
		if s.bumpActive != 1 {
			t.Fatalf("tick %d: bump deactivated early (remaining was %d)", i, s.bumpRemaining)
		}
	}
	// 5th (final) bump tick → revert. Phase A restores 43200; Phase B
	// (adaptive, no progress, streak from restored 0 → 1 < threshold 10)
	// leaves cooldown at 43200.
	id := "auto-revert-tick-final"
	insertBumpTickRow(t, db, "auto-revert", id, 1)
	bumpTick(t, db, "auto-revert", id, noProgressOutcome("auto-revert"))

	s := readBumpState(t, db, "auto-revert")
	if s.bumpActive != 0 {
		t.Fatal("bump still active after final tick")
	}
	if s.cooldown != 43200 {
		t.Fatalf("cooldown = %d, want restored 43200 (idle → decay law resumes where it was)", s.cooldown)
	}
	if s.floor != 21600 || s.ceiling != 604800 {
		t.Fatalf("policy not restored: floor=%d ceiling=%d", s.floor, s.ceiling)
	}
}

// =============================================================================
// 3. Restart survival: bump state is pure DB — re-read keeps it.
// =============================================================================

func TestBump_RestartSurvives(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "survivor", 43200, 21600, 604800, 2)
	if _, err := database.BumpProject(t.Context(), db, "survivor", 5, 7200, "restart test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// Burn two ticks, then "restart" = nothing but a fresh read (the bump
	// lives entirely in the projects row).
	for _, id := range []string{"s-t1", "s-t2"} {
		insertBumpTickRow(t, db, "survivor", id, 1)
		bumpTick(t, db, "survivor", id, noProgressOutcome("survivor"))
	}
	p, err := database.GetProject(t.Context(), db, "survivor")
	if err != nil {
		t.Fatalf("re-read after simulated restart: %v", err)
	}
	if !p.BumpActive || p.BumpRemainingTicks != 3 {
		t.Fatalf("bump lost across restart: active=%v remaining=%d", p.BumpActive, p.BumpRemainingTicks)
	}
	if p.BumpCooldownS != 7200 || p.BumpReason != "restart test" {
		t.Fatalf("bump fields lost: cooldown=%d reason=%q", p.BumpCooldownS, p.BumpReason)
	}
}

// =============================================================================
// 4. Idle bump: bump ticks neither forgive nor punish the pre-bump streak.
// =============================================================================

func TestBump_IdleNoStreakChange(t *testing.T) {
	db := slowdownTestDB(t)
	// A decayed project: streak already past the threshold so one more idle
	// tick would normally escalate.
	bumpTestProject(t, db, "idle-bump", 43200, 21600, 604800, 10)
	if _, err := database.BumpProject(t.Context(), db, "idle-bump", 5, 7200, "idle probe"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("idle-tick-%d", i)
		insertBumpTickRow(t, db, "idle-bump", id, 1)
		bumpTick(t, db, "idle-bump", id, noProgressOutcome("idle-bump"))
	}
	s := readBumpState(t, db, "idle-bump")
	if s.bumpActive != 0 {
		t.Fatal("bump not reverted after 5 idle ticks")
	}
	// Phase A restored streak=10; Phase B evaluated ONE idle tick
	// (streak 10→11, past threshold 10 → escalate 43200→86400).
	// The 4 interior bump ticks did NOT escalate — the post-revert cooldown
	// proves only one escalation happened, not five.
	if s.streak != 11 {
		t.Fatalf("streak = %d, want 11 (saved 10 + exactly one post-revert idle eval)", s.streak)
	}
	if s.cooldown != 86400 {
		t.Fatalf("cooldown = %d, want 86400 (single 2x escalation from restored 43200)", s.cooldown)
	}
}

// =============================================================================
// 5. Real work during bump: progress → adaptive drops cooldown to the floor.
// =============================================================================

func TestBump_RealWorkProgress(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "work-bump", 43200, 21600, 604800, 9)
	if _, err := database.BumpProject(t.Context(), db, "work-bump", 5, 7200, "real work"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}

	// The final bump tick delivers real work: 2 commits (code commits —
	// git measurement falls open without a repo, counting them all) and a
	// board that closed net rows. Workdir has no board file: only the
	// commit signal fires.
	outcome := TickOutcome{Project: "work-bump", Status: TickCompleted, Commits: 2}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("w-idle-%d", i)
		insertBumpTickRow(t, db, "work-bump", id, 1)
		bumpTick(t, db, "work-bump", id, noProgressOutcome("work-bump"))
	}
	id := "w-final"
	insertBumpTickRow(t, db, "work-bump", id, 1)
	bumpTick(t, db, "work-bump", id, outcome)

	s := readBumpState(t, db, "work-bump")
	if s.bumpActive != 0 {
		t.Fatal("bump not reverted after final tick")
	}
	if s.streak != 0 {
		t.Fatalf("streak = %d, want 0 (progress reset)", s.streak)
	}
	// Worked example 1: decayed to 43200, bumped, delivers → lands at the
	// 21600 floor, NOT back at 43200.
	if s.cooldown != 21600 {
		t.Fatalf("cooldown = %d, want 21600 (progress → floor)", s.cooldown)
	}
}

// =============================================================================
// 6. 12h hard cap: an old bump_started_at force-reverts regardless of
//    remaining ticks.
// =============================================================================

func TestBump_12hHardCap(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "capped", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "capped", 5, 7200, "cap test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// Age the bump 13h.
	old := time.Now().UTC().Add(-13 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET bump_started_at = ? WHERE name = 'capped'`, old); err != nil {
		t.Fatalf("age bump: %v", err)
	}
	// Next tick completion — even with 5 remaining — reverts.
	id := "cap-tick"
	insertBumpTickRow(t, db, "capped", id, 1)
	bumpTick(t, db, "capped", id, noProgressOutcome("capped"))

	s := readBumpState(t, db, "capped")
	if s.bumpActive != 0 {
		t.Fatal("bump survived past the 12h hard cap")
	}
	if s.cooldown != 43200 {
		t.Fatalf("cooldown = %d, want restored 43200", s.cooldown)
	}
}

// =============================================================================
// 7. Tick flagging: ticks spawned during a bump carry bump=1.
// =============================================================================

func TestBump_TickFlagged(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "flagged", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "flagged", 5, 7200, "flag test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// markBumpTick runs at spawn time (the row must already exist).
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES ('flag-tick', 'flagged', 'running', datetime('now'))`); err != nil {
		t.Fatalf("insert running tick: %v", err)
	}
	markBumpTick(db, "flagged", "flag-tick")
	var flagged int
	if err := db.QueryRow(`SELECT bump FROM ticks WHERE id = 'flag-tick'`).Scan(&flagged); err != nil {
		t.Fatalf("read tick flag: %v", err)
	}
	if flagged != 1 {
		t.Fatalf("tick bump flag = %d, want 1", flagged)
	}

	// Control: a non-bumped project's tick stays 0.
	bumpTestProject(t, db, "not-bumped", 900, 0, 0, 0)
	if _, err := db.Exec(`INSERT INTO ticks (id, project_name, status, created_at)
		VALUES ('plain-tick', 'not-bumped', 'running', datetime('now'))`); err != nil {
		t.Fatalf("insert plain tick: %v", err)
	}
	markBumpTick(db, "not-bumped", "plain-tick")
	if err := db.QueryRow(`SELECT bump FROM ticks WHERE id = 'plain-tick'`).Scan(&flagged); err != nil {
		t.Fatalf("read plain flag: %v", err)
	}
	if flagged != 0 {
		t.Fatalf("plain tick bump flag = %d, want 0", flagged)
	}
}

// =============================================================================
// 8. Non-bump ticks against a bump-active project do not consume the
//    countdown (only flagged ticks burn bump ticks).
// =============================================================================

func TestBump_OnlyFlaggedTicksConsume(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "race", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "race", 3, 7200, "race test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// A tick spawned just before bump activation (bump=0) completes while
	// the bump is active — countdown must stay at 3.
	insertBumpTickRow(t, db, "race", "race-tick", 0)
	bumpTick(t, db, "race", "race-tick", noProgressOutcome("race"))
	s := readBumpState(t, db, "race")
	if s.bumpActive != 1 || s.bumpRemaining != 3 {
		t.Fatalf("unflagged tick consumed the countdown: active=%d remaining=%d", s.bumpActive, s.bumpRemaining)
	}
}

// =============================================================================
// 9. Packer integration: a bumped project overrides cooldown in selection
//    and carries the bump urgency tier.
// =============================================================================

func TestBump_PackerCooldownOverride(t *testing.T) {
	db := slowdownTestDB(t)
	// Streak already past the threshold (10) so the first idle bump tick
	// ESCALATES cooldown_s mid-bump (7200 → 14400 via the adaptive law,
	// which keeps running during a bump). The packer override is what
	// keeps the bump governing selection despite that escalation — this
	// test fails if the override dispatch is missing.
	bumpTestProject(t, db, "packer-bumped", 43200, 21600, 604800, 10)
	if _, err := database.BumpProject(t.Context(), db, "packer-bumped", 5, 7200, "packer test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// One idle bump tick: adaptive escalates cooldown_s past the bump
	// value (the streak is already at threshold).
	insertBumpTickRow(t, db, "packer-bumped", "pk-escalate", 1)
	bumpTick(t, db, "packer-bumped", "pk-escalate", noProgressOutcome("packer-bumped"))
	var escalated int
	if err := db.QueryRow(`SELECT cooldown_s FROM projects WHERE name = 'packer-bumped'`).Scan(&escalated); err != nil {
		t.Fatalf("read escalated cooldown: %v", err)
	}
	if escalated <= 7200 {
		t.Fatalf("precondition failed: cooldown_s = %d, want > 7200 (adaptive escalation mid-bump)", escalated)
	}
	// Last tick completed 3h ago: inside the escalated cooldown, but
	// OUTSIDE the 7200s bump cooldown → must STILL be selected, because
	// the bump owns the effective cooldown while active.
	last := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = 'packer-bumped'`, last); err != nil {
		t.Fatalf("set last_tick_completed: %v", err)
	}

	calc := NewUrgencyCalculator(time.Minute, 24*time.Hour, 10)
	pk := NewPacker(db, calc, 100, 4, nil)
	pk.SetPendingCounter(NewPendingTaskCounter(0))
	picked, err := pk.Pick(time.Now(), map[string]bool{})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	found := false
	for _, pp := range picked {
		if pp.Name == "packer-bumped" {
			found = true
			if pp.Urgency < bumpBoostUrgency {
				t.Fatalf("bumped project urgency = %v, want >= bump tier %v", pp.Urgency, bumpBoostUrgency)
			}
		}
	}
	if !found {
		t.Fatal("bumped project not selected despite bump cooldown elapsed (3h > 7200s)")
	}

	// Control inside the bump window too: last tick 1h ago (< 7200s) → NOT selected.
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = 'packer-bumped'`,
		time.Now().UTC().Add(-1*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("reset last_tick_completed: %v", err)
	}
	picked, err = pk.Pick(time.Now(), map[string]bool{})
	if err != nil {
		t.Fatalf("Pick 2: %v", err)
	}
	for _, pp := range picked {
		if pp.Name == "packer-bumped" {
			t.Fatal("bumped project selected inside its bump cooldown window (1h < 7200s)")
		}
	}
}

// =============================================================================
// 10. Multi-pool Pack path: bump override + urgency in namespace mode.
// =============================================================================

func TestBump_MultiPoolPackOverride(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "ns-bumped", 43200, 21600, 604800, 0)
	if _, err := database.BumpProject(t.Context(), db, "ns-bumped", 5, 7200, "ns test"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	// Put it in a namespace.
	if _, err := db.Exec(`INSERT INTO namespaces (id, weight, reserved, hard_cap, enabled, created_at, updated_at)
		VALUES ('test-ns', 50, 10, 100, 1, datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("insert namespace: %v", err)
	}
	if _, err := db.Exec(`UPDATE projects SET namespace_id = 'test-ns' WHERE name = 'ns-bumped'`); err != nil {
		t.Fatalf("assign namespace: %v", err)
	}
	last := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE projects SET last_tick_completed = ? WHERE name = 'ns-bumped'`, last); err != nil {
		t.Fatalf("set last_tick_completed: %v", err)
	}

	ctx := t.Context()
	nss, err := database.ListNamespaces(ctx, db, false)
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	projs, err := database.ListProjects(ctx, db, false)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	calc := NewUrgencyCalculator(time.Minute, 24*time.Hour, 10)
	mp := NewMultiPoolPacker(100, 4, nil)
	mp.SetPendingCounter(NewPendingTaskCounter(0))
	res := mp.Pack(projs, nss, calc, map[string]time.Time{"ns-bumped": time.Now().UTC().Add(-3 * time.Hour)}, nil, time.Now())
	found := false
	for _, pp := range res.Projects {
		if pp.Name == "ns-bumped" {
			found = true
			if pp.Urgency < bumpBoostUrgency {
				t.Fatalf("ns-bumped urgency = %v, want >= bump tier", pp.Urgency)
			}
		}
	}
	if !found {
		t.Fatal("bumped project not selected by multi-pool Pack despite bump cooldown elapsed")
	}
}

// =============================================================================
// 11. Manual clear (unbump): Phase A only, restores verbatim.
// =============================================================================

func TestBump_ManualClear(t *testing.T) {
	db := slowdownTestDB(t)
	bumpTestProject(t, db, "manual", 43200, 21600, 604800, 7)
	if _, err := database.BumpProject(t.Context(), db, "manual", 5, 7200, "manual clear"); err != nil {
		t.Fatalf("BumpProject: %v", err)
	}
	if err := database.ClearBump(t.Context(), db, "manual"); err != nil {
		t.Fatalf("ClearBump: %v", err)
	}
	s := readBumpState(t, db, "manual")
	if s.bumpActive != 0 {
		t.Fatal("bump still active after ClearBump")
	}
	if s.cooldown != 43200 || s.streak != 7 || s.floor != 21600 || s.ceiling != 604800 {
		t.Fatalf("manual clear restore wrong: cd=%d streak=%d floor=%d ceiling=%d", s.cooldown, s.streak, s.floor, s.ceiling)
	}
	// Clearing again → ErrNoActiveBump.
	if err := database.ClearBump(t.Context(), db, "manual"); err == nil {
		t.Fatal("second ClearBump should fail with ErrNoActiveBump")
	}
}
