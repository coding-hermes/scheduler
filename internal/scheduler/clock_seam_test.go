package scheduler

import (
	"database/sql"
	"sort"
	"testing"
	"time"
)

// ADV-R04 / G6 clock-seam tests. evaluate() reads its decision instant
// through Loop.nowFn (default time.Now); pinning the seam to a fixed
// instant makes the whole selection deterministic — same DB, same fleet,
// same exact answer on every run. No wall-clock bounds, no sleeps: the
// assertions are exact set equalities at one constructed instant.
//
// The fixed instant is 2031-05-04T03:02:01Z — far on the future side of
// any real wall clock. That directionality is what makes the seam
// load-bearing: if evaluate() bypasses the seam and reads time.Now()
// directly (the pre-ADV-R04 code), every fixture last_tick_completed
// below lies in the FUTURE relative to real time, cooldown comparisons
// invert (negative age is always "< cooldown"), and the exact-set
// assertion fails.

// fixedEvalNow returns the constant evaluation instant every test here
// pins the loop's clock to. Constructed, never read from the wall clock.
func fixedEvalNow() time.Time {
	return time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC)
}

// simSelectedProjects returns the sorted project names evaluate()
// selected, observed through the simulation-mode tick rows: every
// selected project gets exactly one sim- tick row inserted synchronously
// inside evaluate() (SimSpawner.Spawn, DOGFOOD-007), before any simulated
// completion goroutine lands — so the set is a stable, exact observable
// of the selection decision.
func simSelectedProjects(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT DISTINCT project_name FROM ticks WHERE id LIKE 'sim-%'`)
	if err != nil {
		t.Fatalf("query sim ticks: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan sim tick project: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter sim ticks: %v", err)
	}
	sort.Strings(names)
	return names
}

// namesEqual reports exact slice equality (order and content).
func namesEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestClockSeam_ExactSelection is the determinism proof: with the clock
// pinned, evaluate() must select EXACTLY the projects whose cooldowns
// have elapsed at the fixed instant.
//
// Fixture (equal priorities, zero failures, no bumps — cooldown age is
// the only discriminator; every age is exact at fixedNow):
//
//	seamA: cd=60s,   last=fixedNow-90s   → SELECTED   (90s >= 60s)
//	seamB: cd=3600s, last=fixedNow-3599s → skipped    (3599s < 3600s — one second inside cooldown)
//	seamC: cd=300s,  last=fixedNow-600s  → SELECTED   (600s >= 300s; age == 2x cooldown, so NOT
//	                                                 strictly overdue — plain greedy eligibility,
//	                                                 not the GAP-011 force-select)
//	seamD: cd=45s,   last=fixedNow-45s   → SELECTED   (exactly at the boundary: age == cooldown)
func TestClockSeam_ExactSelection(t *testing.T) {
	fixedNow := fixedEvalNow()
	db := newTestDB(t)
	insertEligibilityProject(t, db, "seamA", 60, 5, 0, fixedNow.Add(-90*time.Second), 0, 0)
	insertEligibilityProject(t, db, "seamB", 3600, 5, 0, fixedNow.Add(-3599*time.Second), 0, 0)
	insertEligibilityProject(t, db, "seamC", 300, 5, 0, fixedNow.Add(-600*time.Second), 0, 0)
	insertEligibilityProject(t, db, "seamD", 45, 5, 0, fixedNow.Add(-45*time.Second), 0, 0)

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)                             // simulated spawns insert tick rows; the real spawner is never touched
	l.SetClock(func() time.Time { return fixedNow }) // pin the decision instant

	l.evaluate()

	got := simSelectedProjects(t, db)
	want := []string{"seamA", "seamC", "seamD"}
	if !namesEqual(got, want) {
		t.Fatalf("evaluate() at the fixed clock selected %v, want exactly %v", got, want)
	}
}

// TestClockSeam_AllInCooldownSelectsNothing is the complementary case:
// the fixed clock set so every project sits comfortably inside its
// cooldown must yield the EMPTY selection — no simulated tick rows and
// no tick rows of any kind (nothing spawned, nothing queued).
func TestClockSeam_AllInCooldownSelectsNothing(t *testing.T) {
	fixedNow := fixedEvalNow()
	db := newTestDB(t)
	insertEligibilityProject(t, db, "coolA", 3600, 5, 0, fixedNow.Add(-60*time.Second), 0, 0)
	insertEligibilityProject(t, db, "coolB", 7200, 5, 0, fixedNow.Add(-30*time.Second), 0, 0)

	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetSimulation(1.0)
	l.SetClock(func() time.Time { return fixedNow })

	l.evaluate()

	if got := simSelectedProjects(t, db); len(got) != 0 {
		t.Fatalf("evaluate() with every project in cooldown selected %v, want the empty set", got)
	}
	var ticks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ticks`).Scan(&ticks); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if ticks != 0 {
		t.Fatalf("ticks rows = %d, want 0", ticks)
	}
}

// TestClockSeam_DefaultIsInstalled pins the construction contract: NewLoop
// installs a non-nil seam (time.Now), so production behavior is identical
// to the pre-seam direct read and evaluate() never needs a fallback.
func TestClockSeam_DefaultIsInstalled(t *testing.T) {
	l := NewLoop(newTestDB(t), 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.mu.Lock()
	seam := l.nowFn
	l.mu.Unlock()
	if seam == nil {
		t.Fatal("NewLoop left the clock seam nil — the default time.Now seam must be installed at construction")
	}
}

// TestClockSeam_SetClockNilKeepsSeam pins the nil guard: SetClock(nil)
// must not clear the seam (a nil seam would silently unpin a test's
// fixed clock and fall back to the wall clock).
func TestClockSeam_SetClockNilKeepsSeam(t *testing.T) {
	fixedNow := fixedEvalNow()
	l := NewLoop(newTestDB(t), 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.SetClock(func() time.Time { return fixedNow })
	l.SetClock(nil)
	l.mu.Lock()
	got := l.nowLocked()
	l.mu.Unlock()
	if !got.Equal(fixedNow) {
		t.Fatalf("after SetClock(nil) the seam returns %v, want the previously installed fixed instant %v", got, fixedNow)
	}
}
