package scheduler

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/config"
)

// GAP-043 zero-select monitoring tests. Evaluations log nothing when they
// pick zero projects, so an operator cannot distinguish "evaluating" from
// "evaluating nothing" — observed 2026-08-13 20:55-21:08Z (evals every
// ~5 min, last "EVAL: N selected" line 15:55:06, eligible projects
// present). noteZeroSelect accumulates consecutive zero-selects (with
// eligible projects) and emits a HIGH event + log line at threshold.

func insertTestProject(t *testing.T, db *sql.DB, name string, cooldown int, enabled bool, lastCompleted string) {
	t.Helper()
	e := 0
	if enabled {
		e = 1
	}
	_, err := db.Exec(
		`INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at, last_tick_completed)
		 VALUES (?, 'https://example.com/' || ?, '/tmp/' || ?, 10, 5, ?, 1.0, 'm', 'p', ?, datetime('now'), datetime('now'), ?)`,
		name, name, name, cooldown, e, lastCompleted)
	if err != nil {
		t.Fatalf("insert test project %s: %v", name, err)
	}
}

func assertZeroSelectEventCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE severity = 'HIGH' AND component = 'loop' AND message LIKE 'evaluation selected 0 projects%'`,
	).Scan(&n); err != nil {
		t.Fatalf("count zero-select events: %v", err)
	}
	if n != want {
		t.Fatalf("HIGH zero-select events = %d, want %d", n, want)
	}
}

// TestZeroSelect_NoEligibleProjectsIsNormal: an empty pick with nothing
// eligible (fleet idle) resets the counter and emits nothing.
func TestZeroSelect_NoEligibleProjectsIsNormal(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	l.noteZeroSelect(time.Now(), map[string]bool{})
	l.noteZeroSelect(time.Now(), map[string]bool{})

	if l.zeroSelectCount != 0 {
		t.Fatalf("zeroSelectCount = %d, want 0 (no eligible projects = normal idle)", l.zeroSelectCount)
	}
	assertZeroSelectEventCount(t, db, 0)
}

// TestZeroSelect_ThresholdEmitsEvent: two consecutive zero-selects with an
// eligible project present fire the HIGH event (within ~2 eval cycles).
func TestZeroSelect_ThresholdEmitsEvent(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "km", 900, true, "") // enabled, never completed = eligible
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	l.noteZeroSelect(time.Now(), map[string]bool{})
	assertZeroSelectEventCount(t, db, 0) // first zero-select: below threshold

	l.noteZeroSelect(time.Now(), map[string]bool{})
	assertZeroSelectEventCount(t, db, 1) // second consecutive: event fires

	if l.zeroSelectCount != 2 {
		t.Fatalf("zeroSelectCount = %d, want 2", l.zeroSelectCount)
	}
	if l.zeroSelectEligible != 1 {
		t.Fatalf("zeroSelectEligible = %d, want 1", l.zeroSelectEligible)
	}
}

// TestZeroSelect_SelectResetsCounter: a real selection resets the
// consecutive counter so the anomaly must re-accumulate.
func TestZeroSelect_SelectResetsCounter(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "km", 900, true, "")
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	l.noteZeroSelect(time.Now(), map[string]bool{}) // 1
	l.resetZeroSelect()                             // a selection happened
	l.noteZeroSelect(time.Now(), map[string]bool{}) // back to 1 — not 2 consecutive

	assertZeroSelectEventCount(t, db, 0)
	if l.zeroSelectCount != 1 {
		t.Fatalf("zeroSelectCount = %d, want 1", l.zeroSelectCount)
	}
}

// TestZeroSelect_ReEmitThrottled: the event re-emits at most once per
// zeroSelectReEmitGap while the condition persists.
func TestZeroSelect_ReEmitThrottled(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "km", 900, true, "")
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	now := time.Now()
	l.noteZeroSelect(now, map[string]bool{})
	l.noteZeroSelect(now, map[string]bool{})
	assertZeroSelectEventCount(t, db, 1)

	// Immediate third zero-select: throttled, no new event.
	l.noteZeroSelect(now, map[string]bool{})
	assertZeroSelectEventCount(t, db, 1)

	// After the re-emit gap: a new event fires.
	l.noteZeroSelect(now.Add(zeroSelectReEmitGap+time.Minute), map[string]bool{})
	assertZeroSelectEventCount(t, db, 2)
}

// TestZeroSelect_EligibleExcludesRunningAndCooldown: countEligibleProjects
// counts only enabled projects that are not running and whose cooldown has
// elapsed.
func TestZeroSelect_EligibleExcludesRunningAndCooldown(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "never-done", 900, true, "")                                                            // eligible: never completed
	insertTestProject(t, db, "in-cooldown", 900, true, time.Now().Add(-100*time.Second).UTC().Format(time.RFC3339))  // NOT eligible: cooldown not elapsed
	insertTestProject(t, db, "cooldown-elapsed", 900, true, time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339)) // eligible
	insertTestProject(t, db, "disabled", 900, false, "")                                                             // NOT eligible: disabled
	insertTestProject(t, db, "running-now", 900, true, "")                                                           // NOT eligible: running

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	got := l.countEligibleProjects(time.Now(), map[string]bool{"running-now": true})
	if got != 2 {
		t.Fatalf("countEligibleProjects = %d, want 2 (never-done + cooldown-elapsed)", got)
	}
}

// TestZeroSelect_StatsExposed: ZeroSelectStats surfaces the diagnostics for
// /api/v1/status.
func TestZeroSelect_StatsExposed(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "km", 900, true, "")
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	c, e, last := l.ZeroSelectStats()
	if c != 0 || e != 0 || last != "" {
		t.Fatalf("initial stats = (%d, %d, %q), want (0, 0, \"\")", c, e, last)
	}

	l.noteZeroSelect(time.Now(), map[string]bool{})
	l.noteZeroSelect(time.Now(), map[string]bool{})

	c, e, last = l.ZeroSelectStats()
	if c != 2 || e != 1 {
		t.Fatalf("stats after two zero-selects = (count %d, eligible %d), want (2, 1)", c, e)
	}
	if last == "" {
		t.Fatal("lastEvent = \"\", want a timestamp after event emit")
	}
	// Zero-select events carry the eligible count in details.
	var details string
	if err := db.QueryRow(
		`SELECT details FROM events WHERE severity = 'HIGH' AND component = 'loop' AND message LIKE 'evaluation selected 0 projects%' LIMIT 1`,
	).Scan(&details); err != nil {
		t.Fatalf("read event details: %v", err)
	}
	if !strings.Contains(details, `"eligible":1`) {
		t.Fatalf("event details missing eligible count: %s", details)
	}
}

// insertTestProjectWithFailures is insertTestProject plus a
// consecutive_failures counter (S-GAP-001 spawn-failure backoff).
func insertTestProjectWithFailures(t *testing.T, db *sql.DB, name string, cooldown int, lastCompleted string, failures int) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO projects (name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, enabled, created_at, updated_at, last_tick_completed, consecutive_failures)
		 VALUES (?, 'https://example.com/' || ?, '/tmp/' || ?, 10, 5, ?, 1.0, 'm', 'p', 1, datetime('now'), datetime('now'), ?, ?)`,
		name, name, name, cooldown, lastCompleted, failures)
	if err != nil {
		t.Fatalf("insert test project %s: %v", name, err)
	}
}

// TestZeroSelect_FailureBackoffNotEligible (GAP-050): a project whose raw
// cooldown has elapsed but whose FailureBackoff cooldown (consecutive
// failures) has NOT elapsed is not eligible — the packer would skip it, so
// a zero select must not alarm. 900s cooldown with 3 failures backs off to
// 900s * 2^2 = 3600s; 30min since completion elapses the raw cooldown but
// not the backoff.
func TestZeroSelect_FailureBackoffNotEligible(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	insertTestProjectWithFailures(t, db, "flaky", 900, now.Add(-30*time.Minute).UTC().Format(time.RFC3339), 3)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	if got := l.countEligibleProjects(now, map[string]bool{}); got != 0 {
		t.Fatalf("countEligibleProjects = %d, want 0 (failure backoff not elapsed)", got)
	}

	l.noteZeroSelect(now, map[string]bool{})
	l.noteZeroSelect(now, map[string]bool{})
	assertZeroSelectEventCount(t, db, 0)
	if l.zeroSelectCount != 0 {
		t.Fatalf("zeroSelectCount = %d, want 0", l.zeroSelectCount)
	}
}

// TestZeroSelect_BlackoutMultiplierNotEligible (GAP-050): inside a
// blackout window (06:00-10:00 UTC, multiplier 2.0) a project whose raw
// cooldown elapsed but whose doubled cooldown has NOT elapsed is not
// eligible. 900s cooldown doubled = 1800s; 25min since completion elapses
// the raw cooldown but not the doubled one.
func TestZeroSelect_BlackoutMultiplierNotEligible(t *testing.T) {
	db := newTestDB(t)
	now := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC) // 08:00 UTC — inside 06:00-10:00
	insertTestProject(t, db, "peak", 900, true, now.Add(-25*time.Minute).UTC().Format(time.RFC3339))
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetBlackoutWindows([]config.BlackoutWindow{
		{Start: "06:00", End: "10:00", Multiplier: 2.0},
	})

	if got := l.countEligibleProjects(now, map[string]bool{}); got != 0 {
		t.Fatalf("countEligibleProjects = %d, want 0 (blackout-doubled cooldown not elapsed)", got)
	}

	l.noteZeroSelect(now, map[string]bool{})
	l.noteZeroSelect(now, map[string]bool{})
	assertZeroSelectEventCount(t, db, 0)
	if l.zeroSelectCount != 0 {
		t.Fatalf("zeroSelectCount = %d, want 0", l.zeroSelectCount)
	}
}

// TestZeroSelect_SaturatedNoEvent (GAP-050): when every slot is busy
// (len(runningSet) >= maxConcurrent) a zero select is expected — the
// packer's global maxConcurrent cap breaks before any selection. Even with
// an eligible project present, no HIGH event may fire.
func TestZeroSelect_SaturatedNoEvent(t *testing.T) {
	db := newTestDB(t)
	insertTestProject(t, db, "km", 900, true, "") // enabled, never completed = eligible
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	running := map[string]bool{"a": true, "b": true, "c": true, "d": true} // 4/4 slots busy
	l.noteZeroSelect(time.Now(), running)
	l.noteZeroSelect(time.Now(), running)

	assertZeroSelectEventCount(t, db, 0)
	if l.zeroSelectCount != 0 {
		t.Fatalf("zeroSelectCount = %d, want 0 (saturation is expected)", l.zeroSelectCount)
	}
}

// TestZeroSelect_GenuineAnomalyStillFires (GAP-050): with no blackout
// windows, no failures and free slots, a project whose cooldown has
// genuinely elapsed IS eligible — the anomaly path must still alarm after
// the threshold.
func TestZeroSelect_GenuineAnomalyStillFires(t *testing.T) {
	db := newTestDB(t)
	now := time.Now()
	insertTestProject(t, db, "km", 900, true, now.Add(-2*time.Hour).UTC().Format(time.RFC3339)) // cooldown elapsed
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	if got := l.countEligibleProjects(now, map[string]bool{}); got != 1 {
		t.Fatalf("countEligibleProjects = %d, want 1", got)
	}

	l.noteZeroSelect(now, map[string]bool{})
	assertZeroSelectEventCount(t, db, 0) // below threshold

	l.noteZeroSelect(now, map[string]bool{})
	assertZeroSelectEventCount(t, db, 1) // second consecutive: event fires

	if l.zeroSelectCount != 2 {
		t.Fatalf("zeroSelectCount = %d, want 2", l.zeroSelectCount)
	}
}
