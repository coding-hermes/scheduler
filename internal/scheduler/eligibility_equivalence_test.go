package scheduler

import (
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
)

// insertEligibilityProject inserts an enabled project with full control of
// every column the eligibility predicate consumes: cooldown_s, priority,
// consecutive_failures, last_tick_completed, and the SCHED-GAP-107 bump pair.
func insertEligibilityProject(t *testing.T, db *sql.DB, name string, cooldownS, priority, failures int, lastCompleted time.Time, bumpActive, bumpCooldownS int) {
	t.Helper()
	last := ""
	if !lastCompleted.IsZero() {
		last = lastCompleted.UTC().Format(time.RFC3339)
	}
	_, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 model, provider, enabled, created_at, updated_at,
		 last_tick_completed, consecutive_failures,
		 bump_active, bump_cooldown_s)
		VALUES (?, ?, ?, 10, ?, ?, 1.0, 'm', 'p', 1,
		 datetime('now'), datetime('now'), ?, ?, ?, ?)`,
		name, "https://example.com/"+name, "/tmp/"+name,
		priority, cooldownS, last, failures, bumpActive, bumpCooldownS)
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

// insertEligibilityFleet seeds the ADV-R03 fixture fleet. The expected
// eligibility of each member (verified against the shared predicate below):
//
//	projA: cd=60s, 0 failures, last=now-90s  → eligible (90s >= 60s)
//	projB: cd=60s, 2 failures, last=now-30s  → SKIPPED  (FailureBackoff(60s,2)=120s > 30s)
//	projC: cd=60s, 5 failures, last=now-10s  → SKIPPED  (FailureBackoff(60s,5)=960s > 10s)
//	projD: cd=3600s, bump(cd=10s), last=now-12s → eligible via bump (12s >= 10s;
//	       without the bump 12s < 3600s would skip — this row proves BOTH
//	       sites consume bump_cooldown_s, not cooldown_s)
//
// (Brief deltas, arithmetic-forced: projA's last must be ≥60s old and
// projD's ≥10s old for the stated "eligible" outcomes; 90s/12s satisfy.)
func insertEligibilityFleet(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	insertEligibilityProject(t, db, "projA", 60, 5, 0, now.Add(-90*time.Second), 0, 0)
	insertEligibilityProject(t, db, "projB", 60, 5, 2, now.Add(-30*time.Second), 0, 0)
	insertEligibilityProject(t, db, "projC", 60, 5, 5, now.Add(-10*time.Second), 0, 0)
	insertEligibilityProject(t, db, "projD", 3600, 5, 0, now.Add(-12*time.Second), 1, 10)
}

var eligibilityFleet = []string{"projA", "projB", "projC", "projD"}

// packerEligibleSet runs the REAL production packer path (Loop's own packer,
// Pick: SQL scan → bump fold → sort → overdue/cooldown gates) and returns
// which fixture projects it would spawn.
func packerEligibleSet(t *testing.T, l *Loop, now time.Time) map[string]bool {
	t.Helper()
	packed, err := l.packer.Pick(now, nil)
	if err != nil {
		t.Fatalf("packer Pick: %v", err)
	}
	got := make(map[string]bool, len(packed))
	for _, pp := range packed {
		got[pp.Name] = true
	}
	return got
}

// watchdogEligibleSet isolates each fixture project through the REAL
// countEligibleProjects SQL mirror: marking every OTHER project as running
// leaves the one under test as the sole candidate, so the returned count is
// its per-project eligibility (0 or 1).
func watchdogEligibleSet(t *testing.T, l *Loop, now time.Time) map[string]bool {
	t.Helper()
	got := make(map[string]bool, len(eligibilityFleet))
	for _, name := range eligibilityFleet {
		others := make(map[string]bool, len(eligibilityFleet)-1)
		for _, o := range eligibilityFleet {
			if o != name {
				others[o] = true
			}
		}
		got[name] = l.countEligibleProjects(now, others) == 1
	}
	return got
}

// TestEligibilityEquivalence_BumpBackoffBlackout is the GAP-050 drift
// killer (ADV-R03 / G5): the watchdog's SQL-driven eligibility mirror must
// agree, project by project, with the packer's selection path on a fleet
// exercising an active bump, S-GAP-001 failure backoff (two magnitudes),
// and a live blackout window. Before the single-sourced effectiveCooldown
// the two sites carried five hand-copied versions of this arithmetic and
// drifted silently; any future re-inlining that changes one side breaks
// this test.
func TestEligibilityEquivalence_BumpBackoffBlackout(t *testing.T) {
	calc := NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)

	// Pinned premises — the backoff arithmetic the fixture relies on.
	if d, skip := effectiveCooldown(60, 5, 2, nil, time.Now(), calc); skip || d != 120*time.Second {
		t.Fatalf("effectiveCooldown(60s,2 failures) = %v skip=%v, want 120s (60<<1)", d, skip)
	}
	if d, skip := effectiveCooldown(60, 5, 5, nil, time.Now(), calc); skip || d != 960*time.Second {
		t.Fatalf("effectiveCooldown(60s,5 failures) = %v skip=%v, want 960s (60<<4)", d, skip)
	}

	// --- Phase 1: no blackout ---
	now := time.Now().UTC().Truncate(time.Second)
	db := slowdownTestDB(t)
	insertEligibilityFleet(t, db, now)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	packer := packerEligibleSet(t, l, now)
	watchdog := watchdogEligibleSet(t, l, now)
	want := map[string]bool{"projA": true, "projB": false, "projC": false, "projD": true}
	assertEligibilityAgreement(t, "no-blackout", packer, watchdog, want)

	// --- Phase 2: live blackout window, multiplier 0.5 ---
	// Window built around `now` (±1h, overnight-safe via ActiveMultiplier's
	// end<start → +24h rule) so the test never flakes at window edges.
	now2 := time.Now().UTC().Truncate(time.Second)
	db2 := slowdownTestDB(t)
	insertEligibilityFleet(t, db2, now2)
	l2 := NewLoop(db2, 30*time.Second, 24*time.Hour, 10, 100, 4)
	windows := []config.BlackoutWindow{{
		Start:      now2.Add(-time.Hour).Format("15:04"),
		End:        now2.Add(time.Hour).Format("15:04"),
		Multiplier: 0.5,
	}}
	l2.SetBlackoutWindows(windows)

	// Direct value pin: a 0.5 multiplier must NOT shrink the cooldown —
	// only mult > 1.0 applies — so projA's composed cooldown stays 60s.
	if d, skip := effectiveCooldown(60, 5, 0, windows, now2, l2.calculator); skip || d != 60*time.Second {
		t.Fatalf("blackout 0.5: effectiveCooldown = %v skip=%v, want 60s (sub-1.0 multipliers are no-ops)", d, skip)
	}

	// Both sites must compute the SAME multiplied cooldown for every
	// project crossing both eligibility tests (projA eligible, projB/C
	// backoff-skipped, projD bump-eligible — all under the live window).
	packer2 := packerEligibleSet(t, l2, now2)
	watchdog2 := watchdogEligibleSet(t, l2, now2)
	assertEligibilityAgreement(t, "blackout-0.5", packer2, watchdog2, want)
}

// assertEligibilityAgreement fails on any per-project divergence between
// the packer and the watchdog (the GAP-050 drift class) or on any mismatch
// with the expected eligible set.
func assertEligibilityAgreement(t *testing.T, phase string, packer, watchdog, want map[string]bool) {
	t.Helper()
	for _, name := range eligibilityFleet {
		if packer[name] != watchdog[name] {
			t.Fatalf("%s: GAP-050 drift on %s — packer eligible=%v, watchdog eligible=%v",
				phase, name, packer[name], watchdog[name])
		}
		if packer[name] != want[name] {
			t.Fatalf("%s: %s eligible=%v, want %v", phase, name, packer[name], want[name])
		}
	}
}
