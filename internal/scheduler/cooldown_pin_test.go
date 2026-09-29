package scheduler

import (
	"database/sql"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/config"
)

// SCHED-GAP-1661: pin-first effective cooldown. The operator pin
// (projects.cooldown_pin_s) must outrank cooldown_s, the priority-derived
// dynamic interval and the bump (folded into cooldownS) as the BASE of
// effectiveCooldown — while the failure backoff and the blackout multiplier
// still compose ON TOP of the pin base. Resolution order:
// pin > cooldown_s > dynamic interval.

func pinOf(v int) *int { return &v }

// TestEffectiveCooldown_PinFirstResolution pins every branch of the
// pin-first resolution directly on the shared predicate.
func TestEffectiveCooldown_PinFirstResolution(t *testing.T) {
	calc := NewUrgencyCalculator(30*time.Second, 24*time.Hour, 10)
	week := 604800 * time.Second
	day := 86400 * time.Second

	// Blackout windows built around now (±1h) so the test never flakes at
	// the window edges (same shape as TestEligibilityEquivalence_BumpBackoffBlackout).
	// MUST be UTC-located: ActiveMultiplier builds the window at UTC wall
	// clock, so a local `now` formats a wall time the UTC window misses.
	now := time.Now().UTC()
	inWindow := func(mult float64) []config.BlackoutWindow {
		return []config.BlackoutWindow{{
			Start:      now.Add(-time.Hour).Format("15:04"),
			End:        now.Add(time.Hour).Format("15:04"),
			Multiplier: mult,
		}}
	}

	cases := []struct {
		name      string
		cooldownS int
		pin       *int
		failures  int
		windows   []config.BlackoutWindow
		want      time.Duration
		wantSkip  bool
	}{
		{
			name:      "pin beats cooldown_s (604800 pin over 86400 cooldown)",
			cooldownS: 86400, pin: pinOf(604800),
			want: week,
		},
		{
			name:      "pin beats the dynamic interval (cooldown_s=0)",
			cooldownS: 0, pin: pinOf(604800),
			want: week,
		},
		{
			name:      "pin beats the bump (bumped cooldown folded into cooldownS)",
			cooldownS: 10, pin: pinOf(604800),
			want: week,
		},
		{
			name:      "nil pin falls through to cooldown_s",
			cooldownS: 86400, pin: nil,
			want: day,
		},
		{
			name:      "zero pin falls through to cooldown_s",
			cooldownS: 86400, pin: pinOf(0),
			want: day,
		},
		{
			name:      "negative pin falls through to cooldown_s",
			cooldownS: 86400, pin: pinOf(-7),
			want: day,
		},
		{
			name:      "pin + 1 failure keeps the pin (first failure costs nothing)",
			cooldownS: 86400, pin: pinOf(604800), failures: 1,
			want: week,
		},
		{
			name:      "pin + 2 failures backs off on a small pin base",
			cooldownS: 86400, pin: pinOf(3600), failures: 2,
			want: 7200 * time.Second, // FailureBackoff(3600s, 2) = 3600<<1 = 2h
		},
		{
			name:      "pin + 2 failures clamps at a large pin (cap never below base, never sped up)",
			cooldownS: 86400, pin: pinOf(604800), failures: 2,
			want: week, // FailureBackoff(604800s, 2): 604800<<1 > cap=max(2h,604800s)=604800s → clamped at the pin
		},
		{
			name:      "pin + 5 failures clamps at the pin (cap never below base)",
			cooldownS: 86400, pin: pinOf(604800), failures: 5,
			want: week,
		},
		{
			name:      "pin inside a 2.0 blackout is doubled",
			cooldownS: 86400, pin: pinOf(3600), windows: inWindow(2.0),
			want: 7200 * time.Second,
		},
		{
			name:      "pin inside a 0.5 blackout keeps the pin (sub-1.0 no-op)",
			cooldownS: 86400, pin: pinOf(3600), windows: inWindow(0.5),
			want: 3600 * time.Second,
		},
		{
			name:      "pin inside a skip-mode blackout never becomes eligible",
			cooldownS: 86400, pin: pinOf(3600), windows: inWindow(0),
			wantSkip: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, skip := effectiveCooldown(tc.cooldownS, 5, tc.failures, tc.windows, now, calc, tc.pin)
			if skip != tc.wantSkip {
				t.Fatalf("skipMode = %v, want %v (dur %v)", skip, tc.wantSkip, d)
			}
			if tc.wantSkip {
				return
			}
			if d != tc.want {
				t.Fatalf("effectiveCooldown(cooldown_s=%d, pin=%v, failures=%d) = %v, want %v",
					tc.cooldownS, tc.pin, tc.failures, d, tc.want)
			}
		})
	}

	// Unpinned cooldown_s==0 must still resolve to the dynamic interval —
	// the G5 alignment TestEligibilityEquivalence_BumpBackoffBlackout pins,
	// now explicitly scoped to the UNPINNED arm of the resolution.
	d, skip := effectiveCooldown(0, 5, 0, nil, now, calc, nil)
	if skip || d < time.Minute {
		t.Fatalf("unpinned cooldown_s=0 = %v skip=%v, want the positive dynamic interval", d, skip)
	}
}

// insertPinnedProject seeds one enabled project with full control of every
// column the pin-first predicate consumes: cooldown_s, cooldown_pin_s,
// priority, consecutive_failures and last_tick_completed.
func insertPinnedProject(t *testing.T, db *sql.DB, name string, cooldownS int, pinS *int, failures int, lastCompleted time.Time) {
	t.Helper()
	last := ""
	if !lastCompleted.IsZero() {
		last = lastCompleted.UTC().Format(time.RFC3339)
	}
	_, err := db.Exec(`INSERT INTO projects
		(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
		 cooldown_pin_s, model, provider, enabled, created_at, updated_at,
		 last_tick_completed, consecutive_failures)
		VALUES (?, ?, ?, 10, 5, ?, 1.0,
		 ?, 'm', 'p', 1, datetime('now'), datetime('now'), ?, ?)`,
		name, "https://example.com/"+name, "/tmp/"+name,
		cooldownS, pinS, last, failures)
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
}

// pinFleet is the SCHED-GAP-1661 parity fleet. Each member's expected
// eligibility, verified against the shared predicate:
//
//	pinP: cd=86400, pin=604800, last=now-25h → SKIPPED (25h < 7d pin; the
//	      SCHED-GAP-1661 live shape — before the fix 86400 ≤ 25h made it
//	      eligible and the perf lanes reticked at ~25-30h)
//	pinQ: cd=86400, no pin, last=now-25h     → eligible (25h ≥ 24h — the
//	      SAME lane without the pin; proves the pin, not cooldown_s,
//	      drives the gate)
//	pinR: cd=86400, pin=0, last=now-25h      → eligible (pin column 0 =
//	      NO pin — falls through to cooldown_s exactly like nil)
//	pinS: cd=86400, pin=604800, failures=2, last=now-25h → SKIPPED (the
//	      backoff composes on the pin base: 1209600s ≫ 25h)
var pinFleet = []string{"pinP", "pinQ", "pinR", "pinS"}

func insertPinFleet(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	insertPinnedProject(t, db, "pinP", 86400, pinOf(604800), 0, now.Add(-25*time.Hour))
	insertPinnedProject(t, db, "pinQ", 86400, nil, 0, now.Add(-25*time.Hour))
	insertPinnedProject(t, db, "pinR", 86400, pinOf(0), 0, now.Add(-25*time.Hour))
	insertPinnedProject(t, db, "pinS", 86400, pinOf(604800), 2, now.Add(-25*time.Hour))
}

// pinWatchdogSet isolates each fixture project through the REAL
// countEligibleProjects SQL mirror (same shape as watchdogEligibleSet):
// every OTHER project is marked running, so the count is the project's
// per-project eligibility.
func pinWatchdogSet(t *testing.T, l *Loop, now time.Time) map[string]bool {
	t.Helper()
	got := make(map[string]bool, len(pinFleet))
	for _, name := range pinFleet {
		others := make(map[string]bool, len(pinFleet)-1)
		for _, o := range pinFleet {
			if o != name {
				others[o] = true
			}
		}
		got[name] = l.countEligibleProjects(now, others) == 1
	}
	return got
}

// pinPackerSet runs the REAL production packer path (Pick) over the pin
// fleet. The overdue force-select must stay out of the way: every fixture's
// age (25h) is far below 2x its effective cooldown, so GAP-011 never fires.
func pinPackerSet(t *testing.T, l *Loop, now time.Time) map[string]bool {
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

// assertPinAgreement fails on any per-project divergence between the packer
// and the watchdog (the GAP-050 drift class) or on any mismatch with the
// expected eligible set.
func assertPinAgreement(t *testing.T, phase string, packer, watchdog, want map[string]bool) {
	t.Helper()
	for _, name := range pinFleet {
		if packer[name] != watchdog[name] {
			t.Fatalf("%s: GAP-050 drift on %s — packer eligible=%v, watchdog eligible=%v",
				phase, name, packer[name], watchdog[name])
		}
		if packer[name] != want[name] {
			t.Fatalf("%s: %s eligible=%v, want %v", phase, name, packer[name], want[name])
		}
	}
}

// TestEligibilityEquivalence_CooldownPin is the SCHED-GAP-1661 parity test:
// the watchdog's SQL-driven eligibility mirror must agree with the packer's
// selection path on a PINNED fleet — and the pin must bite at both sites
// identically (watchdog parity, acceptance criterion 2). A second phase
// under a live 2.0 blackout window proves the multiplier composes on the
// pin base at both sites too (a pinned lane inside a peak window is slowed
// on the pin, and pinQ/pinR drop out because 2x24h > 25h).
func TestEligibilityEquivalence_CooldownPin(t *testing.T) {
	// --- Phase 1: no blackout — the pin decides ---
	now := time.Now().UTC().Truncate(time.Second)
	db := slowdownTestDB(t)
	insertPinFleet(t, db, now)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)

	want := map[string]bool{"pinP": false, "pinQ": true, "pinR": true, "pinS": false}
	assertPinAgreement(t, "no-blackout", pinPackerSet(t, l, now), pinWatchdogSet(t, l, now), want)

	// --- Phase 2: live blackout window, multiplier 2.0 — composes on the pin base ---
	now2 := time.Now().UTC().Truncate(time.Second)
	db2 := slowdownTestDB(t)
	insertPinFleet(t, db2, now2)
	l2 := NewLoop(db2, 30*time.Second, 24*time.Hour, 10, 100, 4)
	windows := []config.BlackoutWindow{{
		Start:      now2.Add(-time.Hour).Format("15:04"),
		End:        now2.Add(time.Hour).Format("15:04"),
		Multiplier: 2.0,
	}}
	l2.SetBlackoutWindows(windows)

	// Direct pin: the doubled pin base — 7d * 2 — is what both sites must
	// agree on (the pin does not immunize against the blackout multiplier).
	if d, skip := effectiveCooldown(86400, 5, 0, windows, now2, l2.calculator, pinOf(604800)); skip || d != 2*604800*time.Second {
		t.Fatalf("blackout 2.0 on pin: effectiveCooldown = %v skip=%v, want %v", d, skip, 2*604800*time.Second)
	}

	want2 := map[string]bool{"pinP": false, "pinQ": false, "pinR": false, "pinS": false}
	assertPinAgreement(t, "blackout-2.0", pinPackerSet(t, l2, now2), pinWatchdogSet(t, l2, now2), want2)
}
