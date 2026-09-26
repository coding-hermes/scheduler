package scheduler_test

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// ── SCHED-GAP-1636: spend windows are unchanged by the prefilter rewrite ──
//
// The rewritten LoadBudgetSpends adds a cheap string prefilter to each window
// predicate. These cases are the ones that could break silently if the
// prefilter were stricter than intended. The existing SCHED-GAP-066 budget
// tests all write UTC ('Z') stamps; the LIVE daemon writes RFC3339 in the
// host's local zone (-05:00 on the current fleet), so the offset cases below —
// wall clock on a different calendar day than the UTC instant — are the real
// regression net for the rewrite.
//
// Expectations are computed in Go from each row's INSTANT (never hand-copied
// from the SQL result), so the test states the contract independently of the
// query under test.

// insertOffsetCostTick inserts a tick whose spawned_at is written the way the
// daemon writes it: RFC3339 in a zone with the given offset. A nil instant
// writes NULL, the shape a queued (never-spawned) row carries.
func insertOffsetCostTick(t *testing.T, db *sql.DB, tickID, project string, at *time.Time, offsetSeconds int, cost float64) {
	t.Helper()
	var spawned interface{}
	if at != nil {
		spawned = at.In(time.FixedZone("host", offsetSeconds)).Format(time.RFC3339)
	}
	// created_at is NOT NULL and is not read by the spend aggregate — a queued
	// row simply has no spawned_at.
	createdAt := time.Date(2026, 9, 25, 12, 34, 56, 0, time.UTC).Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, cost_usd, created_at) VALUES (?, ?, 'completed', ?, ?, ?)`,
		tickID, project, spawned, cost, createdAt); err != nil {
		t.Fatalf("insert offset cost tick %s: %v", tickID, err)
	}
}

func TestSchedGap1636_SpendWindowsAcrossZoneOffsets(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const project = "offset-lane"
	mustCreateProjectAt(t, db, project, 10, 5, 900, 1.0)

	now := time.Date(2026, 9, 25, 12, 34, 56, 0, time.UTC) // Thursday
	dayStart := scheduler.UTCDayStart(now)                 // 2026-09-25T00:00Z
	weekStart := scheduler.UTCWeekStart(now)               // 2026-09-21T00:00Z

	h := func(d time.Duration) *time.Time {
		v := now.Add(d)
		return &v
	}
	// The boundary instants themselves are addressed relative to the window
	// starts, not relative to now, so these cases cannot drift with now's
	// time of day.
	at := func(t0 time.Time) *time.Time { return &t0 }

	cases := []struct {
		name    string
		at      *time.Time
		offset  int
		cost    float64
		comment string
	}{
		{"utc-recent", h(-30 * time.Minute), 0, 1.00, "UTC stamp inside both windows"},
		{"local-recent", h(-45 * time.Minute), -5 * 3600, 2.00, "the live daemon's -05:00 stamp"},
		{"east-wall-today", at(dayStart.Add(-13 * time.Hour)), 14 * 3600, 4.00,
			"wall clock reads 2026-09-25+14:00 (today) but the instant is 2026-09-24T11:00Z: weekly only"},
		{"east-exact-day-start", at(dayStart), 14 * 3600, 8.00,
			"instant exactly at the day boundary counts (>= semantics)"},
		{"utc-one-second-before", at(dayStart.Add(-time.Second)), 0, 16.00, "one second early does not count"},
		{"west-week-edge", at(weekStart.Add(-time.Hour)), -12 * 3600, 32.00,
			"wall clock reads 2026-09-20+12:00 (this week's date) but the instant is last week: no window"},
		{"west-exact-week-start", at(weekStart), -12 * 3600, 64.00, "instant exactly at the week boundary counts"},
		{"ancient", at(weekStart.Add(-30 * 24 * time.Hour)), 14 * 3600, 128.00, "outside every window, still in Total"},
		{"queued-null", nil, 0, 256.00, "NULL spawned_at: no window, counted in Total"},
	}

	var wantDaily, wantWeekly, wantTotal float64
	for i, c := range cases {
		insertOffsetCostTick(t, db, "t-off-"+string(rune('a'+i)), project, c.at, c.offset, c.cost)
		if c.at != nil {
			instant := c.at.UTC()
			if !instant.Before(dayStart) {
				wantDaily += c.cost
			}
			if !instant.Before(weekStart) {
				wantWeekly += c.cost
			}
		}
		wantTotal += c.cost
	}

	spends, err := scheduler.LoadBudgetSpends(ctx, db, now)
	if err != nil {
		t.Fatalf("LoadBudgetSpends: %v", err)
	}
	got := spends[project]

	const eps = 1e-9
	if math.Abs(got.Daily-wantDaily) > eps {
		t.Errorf("daily spend = %.2f, want %.2f — a zone-offset row landed in the wrong day window", got.Daily, wantDaily)
	}
	if math.Abs(got.Weekly-wantWeekly) > eps {
		t.Errorf("weekly spend = %.2f, want %.2f — a zone-offset row landed in the wrong week window", got.Weekly, wantWeekly)
	}
	if math.Abs(got.Total-wantTotal) > eps {
		t.Errorf("total spend = %.2f, want %.2f — Total must include every row, NULL spawned_at included", got.Total, wantTotal)
	}

	// A stamp on a different calendar day than its instant is the case a naive
	// string window compare gets wrong. Pin it by asserting the day total is
	// NOT the value a wall-clock-only predicate would produce, so a future
	// "simplification" to string-only predicates fails loudly here instead of
	// silently discounting spend.
	var eastWallCost float64
	var sawEastWall bool
	for _, c := range cases {
		if c.name == "east-wall-today" {
			eastWallCost, sawEastWall = c.cost, true
		}
	}
	if !sawEastWall {
		t.Fatal("east-wall-today case missing from the table — the wall-clock/instant disagreement is no longer covered")
	}
	if wallClockOnlyDaily := wantDaily + eastWallCost; math.Abs(got.Daily-wallClockOnlyDaily) <= eps {
		t.Errorf("daily spend %.2f equals the wall-clock-only total %.2f — the day window is comparing strings, not instants",
			got.Daily, wallClockOnlyDaily)
	}
}
