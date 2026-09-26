package scheduler

import (
	"testing"
	"time"
)

// ── SCHED-GAP-1636: the spend-window string prefilter must be a SUPERSET ──
//
// LoadBudgetSpends ANDs a cheap string comparison in front of the exact
// julianday() window predicate, so most rows never reach the date parser.
// That trick is only safe in one direction: the prefilter may admit rows the
// predicate then discards, but it must NEVER drop a row the predicate keeps.
// A stricter bound would silently under-count daily/weekly spend on a live
// fleet — no error, no log line, just a lane that never gets budget-blocked.
//
// The fleet writes RFC3339 in the host's local zone (live rows carry -05:00)
// but any representable offset can reach the table, so the guard has to be a
// property test over the offset range against the boundary, not a fixture.
func TestSCHEDGAP1636PrefilterIsSupersetOfWindowPredicate(t *testing.T) {
	// Every real-world zone offset, both extremes, a UTC row, and two
	// non-hour offsets (Nepal +05:45, Marquesas -09:30).
	offsets := []int{
		0,
		-5 * 3600,
		-12 * 3600,
		14 * 3600,
		12*3600 + 45*60,
		-(9*3600 + 30*60),
	}
	boundary := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	bound := budgetSpendPrefilterBound(boundary)
	if bound == "" {
		t.Fatal("prefilter bound is empty")
	}

	var inWindow, slackAdmitted int
	for _, off := range offsets {
		zone := time.FixedZone("probe", off)
		// Walk three days either side of the boundary so both sides of the
		// slack window and the exact boundary instant are covered.
		for delta := -72 * time.Hour; delta <= 72*time.Hour; delta += 7 * time.Minute {
			instant := boundary.Add(delta)
			stamp := instant.In(zone).Format(time.RFC3339)
			// The exact predicate's verdict (julianday(spawned_at) >=
			// julianday(boundary), with second granularity).
			kept := !instant.Truncate(time.Second).Before(boundary)
			admitted := stamp >= bound
			if kept {
				if !admitted {
					t.Fatalf("offset %+ds, instant %s: stamp %q is INSIDE the window but the prefilter %q dropped it — "+
						"the rewritten predicate would under-count spend",
						off, instant.UTC().Format(time.RFC3339), stamp, bound)
				}
				inWindow++
				continue
			}
			if admitted {
				slackAdmitted++
			}
		}
	}

	if inWindow == 0 {
		t.Fatal("no in-window stamps were probed — the property held vacuously")
	}
	// If nothing lands in the slack, the bound is as strict as the predicate,
	// which is exactly the state the SQL must NOT be in: the string compare
	// would then be silently load-bearing instead of an optimization.
	if slackAdmitted == 0 {
		t.Fatalf("prefilter %q admitted no out-of-window stamps — it is not a superset, and the julianday() clause "+
			"after it would be dead weight", bound)
	}
	t.Logf("prefilter %q: %d in-window stamps all admitted; %d out-of-window stamps admitted and left to julianday()",
		bound, inWindow, slackAdmitted)
}

// TestSCHEDGAP1636PrefilterSlackCoversZoneExtremes pins the slack constant
// itself: the bound is derived by subtracting budgetSpendWindowSlack, and the
// superset property above only holds while that slack exceeds the largest
// wall-clock lead any zone offset can produce (+14:00), plus room for the
// coarser second-granularity comparison.
func TestSCHEDGAP1636PrefilterSlackCoversZoneExtremes(t *testing.T) {
	const maxZoneLead = 14 * time.Hour
	if budgetSpendWindowSlack <= maxZoneLead {
		t.Fatalf("budgetSpendWindowSlack = %s, must exceed the +14:00 zone maximum (%s) to keep the prefilter a superset",
			budgetSpendWindowSlack, maxZoneLead)
	}
	boundary := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	if got, want := budgetSpendPrefilterBound(boundary), "2026-09-24 00:00:00"; got != want {
		t.Errorf("budgetSpendPrefilterBound(%s) = %q, want %q (boundary minus %s, no zone suffix)",
			boundary.Format(time.RFC3339), got, want, budgetSpendWindowSlack)
	}
}
