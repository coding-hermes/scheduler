package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-17000 — dry-run throttle.
//
// The property under test is the owner's rule: five dry runs must produce a
// warning, and a lane that keeps producing nothing must be SUSPENDED (cooldown
// pinned) rather than spawned again — and must come straight back the moment it
// produces work, without a human and without clearing a pin somebody else set.

func TestDryRunThrottleSettingValidation(t *testing.T) {
	cases := []struct {
		cfg     int
		wantErr bool
		why     string
	}{
		{0, false, "0 means 'use the default', not 'off'"},
		{5, false, "the warn streak is the lowest legal throttle"},
		{10, false, "the documented default"},
		{99, false, "no upper bound"},
		{4, true, "below the warn line would suspend with no warning"},
		{-3, true, "negative is nonsense"},
	}
	for _, c := range cases {
		err := DryRunThrottleSettingError(c.cfg)
		if c.wantErr && err == nil {
			t.Errorf("dry_run_throttle=%d accepted, want rejected (%s)", c.cfg, c.why)
		}
		if !c.wantErr && err != nil {
			t.Errorf("dry_run_throttle=%d rejected (%v), want accepted (%s)", c.cfg, err, c.why)
		}
	}
}

func TestDryRunThrottleLimit(t *testing.T) {
	if got := dryRunThrottleLimit(0); got != DefaultDryRunThrottle {
		t.Errorf("limit(0) = %d, want default %d", got, DefaultDryRunThrottle)
	}
	if got := dryRunThrottleLimit(4); got != DefaultDryRunThrottle {
		t.Errorf("limit(4) = %d, want default %d (below warn streak is not a valid limit)", got, DefaultDryRunThrottle)
	}
	if got := dryRunThrottleLimit(7); got != 7 {
		t.Errorf("limit(7) = %d, want 7", got)
	}
}

func TestDryRunSuspendDue(t *testing.T) {
	if dryRunSuspendDue(DryRunWarnStreak, DefaultDryRunThrottle) {
		t.Errorf("streak %d suspended at limit %d — the warning line must not suspend", DryRunWarnStreak, DefaultDryRunThrottle)
	}
	if !dryRunSuspendDue(DefaultDryRunThrottle, DefaultDryRunThrottle) {
		t.Errorf("streak %d NOT suspended at limit %d — the owner's ten-dry-run rule is the whole feature", DefaultDryRunThrottle, DefaultDryRunThrottle)
	}
	if !dryRunSuspendDue(DefaultDryRunThrottle+3, DefaultDryRunThrottle) {
		t.Error("a streak past the limit must stay suspended")
	}
	if dryRunSuspendDue(3, 0) {
		t.Error("limit 0 must never suspend")
	}
}

func TestDryRunSuspendPinS(t *testing.T) {
	if got := dryRunSuspendPinS(100, 800); got != 800 {
		t.Errorf("pin = %d, want the ceiling 800", got)
	}
	if got := dryRunSuspendPinS(100, 0); got != 800 {
		t.Errorf("pin = %d, want 8x floor 800", got)
	}
	if got := dryRunSuspendPinS(0, 0); got != 0 {
		t.Errorf("pin = %d, want 0 (nothing to pin — a lane with no policy)", got)
	}
}

// newDryRunProject inserts the minimum project row the throttle reads.
func newDryRunProject(t *testing.T, db *sql.DB, name string, streak, floorS, ceilingS int, throttle *int) {
	t.Helper()
	ctx := context.Background()
	p := database.Project{
		Name:      name,
		Workdir:   t.TempDir(),
		Weight:    1,
		Priority:  5,
		CooldownS: floorS,
		Enabled:   true,
		CreatedAt: "2026-11-07T00:00:00Z",
		UpdatedAt: "2026-11-07T00:00:00Z",
	}
	if err := database.CreateProject(ctx, db, &p); err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
	upd := database.ProjectUpdates{
		CooldownFloorS:   &floorS,
		CooldownCeilingS: &ceilingS,
		DryRunThrottle:   throttle,
	}
	if err := database.UpdateProject(ctx, db, name, upd); err != nil {
		t.Fatalf("configure project %s: %v", name, err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE projects SET no_progress_ticks = ? WHERE name = ?`, streak, name); err != nil {
		t.Fatalf("seed streak for %s: %v", name, err)
	}
}

func dryRunRow(t *testing.T, db *sql.DB, name string) (pin sql.NullInt64, by string) {
	t.Helper()
	if err := db.QueryRow(`SELECT cooldown_pin_s, COALESCE(cooldown_pin_by,'') FROM projects WHERE name = ?`, name).Scan(&pin, &by); err != nil {
		t.Fatalf("read pin for %s: %v", name, err)
	}
	return pin, by
}

func dryRunEvents(t *testing.T, db *sql.DB, name, eventType string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE component = 'noop_guard' AND details LIKE ?`,
		"%"+eventType+"%"+name+"%").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func TestDryRunThrottleSuspendsAtLimitAndRecovers(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const lane = "docs-dry"
	newDryRunProject(t, db, lane, DefaultDryRunThrottle, 100, 800, nil)

	action, err := applyDryRunThrottle(ctx, db, lane, false)
	if err != nil {
		t.Fatalf("suspend pass: %v", err)
	}
	if action != dryRunSuspended {
		t.Fatalf("action = %q, want %q at streak %d / limit %d", action, dryRunSuspended, DefaultDryRunThrottle, DefaultDryRunThrottle)
	}
	pin, by := dryRunRow(t, db, lane)
	if !pin.Valid || int(pin.Int64) != 800 {
		t.Fatalf("cooldown_pin_s = %v, want 800 (the ceiling)", pin)
	}
	if by != DryRunThrottlePinBy {
		t.Fatalf("cooldown_pin_by = %q, want %q", by, DryRunThrottlePinBy)
	}
	if n := dryRunEvents(t, db, lane, "dry_run_suspended"); n != 1 {
		t.Fatalf("dry_run_suspended events = %d, want 1", n)
	}

	// Real work arrives: the pin this feature set must be released.
	action, err = applyDryRunThrottle(ctx, db, lane, true)
	if err != nil {
		t.Fatalf("recover pass: %v", err)
	}
	if action != dryRunRecovered {
		t.Fatalf("action = %q, want %q after real work", action, dryRunRecovered)
	}
	pin, by = dryRunRow(t, db, lane)
	if pin.Valid && pin.Int64 != 0 {
		t.Fatalf("cooldown_pin_s = %v after recovery, want cleared", pin)
	}
	if by != "" {
		t.Fatalf("cooldown_pin_by = %q after recovery, want empty", by)
	}
}

func TestDryRunThrottleWarnsAtFive(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const lane = "lint-dry"
	newDryRunProject(t, db, lane, DryRunWarnStreak, 100, 800, nil)

	action, err := applyDryRunThrottle(ctx, db, lane, false)
	if err != nil {
		t.Fatalf("warn pass: %v", err)
	}
	if action != dryRunWarned {
		t.Fatalf("action = %q at streak %d, want %q", action, DryRunWarnStreak, dryRunWarned)
	}
	if n := dryRunEvents(t, db, lane, "dry_run_warn"); n != 1 {
		t.Fatalf("dry_run_warn events = %d, want 1", n)
	}
	pin, _ := dryRunRow(t, db, lane)
	if pin.Valid && pin.Int64 != 0 {
		t.Fatalf("warning must not pin the lane; cooldown_pin_s = %v", pin)
	}
}

func TestDryRunThrottleHonoursCustomSetting(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const lane = "custom-dry"
	cfg := 6
	newDryRunProject(t, db, lane, 6, 100, 800, &cfg)

	action, err := applyDryRunThrottle(ctx, db, lane, false)
	if err != nil {
		t.Fatalf("suspend pass: %v", err)
	}
	if action != dryRunSuspended {
		t.Fatalf("action = %q, want %q at streak 6 with dry_run_throttle=6", action, dryRunSuspended)
	}
}

func TestDryRunThrottleNeverClobbersOperatorPin(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const lane = "op-pinned"
	newDryRunProject(t, db, lane, DefaultDryRunThrottle+5, 100, 800, nil)
	if err := database.UpdateProject(ctx, db, lane, database.ProjectUpdates{CooldownPinS: intPtr(5000)}); err != nil {
		t.Fatalf("set operator pin: %v", err)
	}

	action, err := applyDryRunThrottle(ctx, db, lane, false)
	if err != nil {
		t.Fatalf("suspend pass: %v", err)
	}
	if action != dryRunDeferred {
		t.Fatalf("action = %q, want %q — an operator pin already slows the lane", action, dryRunDeferred)
	}
	pin, by := dryRunRow(t, db, lane)
	if !pin.Valid || int(pin.Int64) != 5000 {
		t.Fatalf("operator pin = %v, want untouched at 5000", pin)
	}
	if by == DryRunThrottlePinBy {
		t.Fatal("operator pin was relabelled as ours — recovery would then clear a pin we never set")
	}

	// And recovery must not clear it either.
	action, err = applyDryRunThrottle(ctx, db, lane, true)
	if err != nil {
		t.Fatalf("recover pass: %v", err)
	}
	if action != dryRunNone {
		t.Fatalf("action = %q on recovery of an operator pin, want none", action)
	}
	pin, _ = dryRunRow(t, db, lane)
	if !pin.Valid || int(pin.Int64) != 5000 {
		t.Fatalf("operator pin after a productive tick = %v, want still 5000", pin)
	}
}

func TestDryRunThrottleWithoutCooldownPolicyDoesNotInventOne(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	const lane = "no-policy"
	newDryRunProject(t, db, lane, DefaultDryRunThrottle+2, 0, 0, nil)

	action, err := applyDryRunThrottle(ctx, db, lane, false)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if action != dryRunNone {
		t.Fatalf("action = %q on a lane with no floor/ceiling, want none (never invent a cooldown)", action)
	}
	if n := dryRunEvents(t, db, lane, "dry_run_suspend_unavailable"); n != 1 {
		t.Fatalf("dry_run_suspend_unavailable events = %d, want 1 (the miss must be visible)", n)
	}
}

func TestDryRunThrottleIsSuspendedByMessage(t *testing.T) {
	// Guard against a future edit that suspends without saying so: the
	// operator-visible message must name the lane and the numbers.
	db := newTestDB(t)
	ctx := context.Background()
	const lane = "msg-dry"
	newDryRunProject(t, db, lane, DefaultDryRunThrottle, 100, 800, nil)
	if _, err := applyDryRunThrottle(ctx, db, lane, false); err != nil {
		t.Fatalf("suspend pass: %v", err)
	}
	var msg string
	if err := db.QueryRow(`SELECT message FROM events WHERE component='noop_guard' AND details LIKE '%dry_run_suspended%' ORDER BY id DESC LIMIT 1`).Scan(&msg); err != nil {
		t.Fatalf("read suspend event: %v", err)
	}
	if !strings.Contains(strings.ToLower(msg), "suspend") {
		t.Fatalf("suspend event message = %q, want it to say the lane was suspended", msg)
	}
}

func intPtr(v int) *int { return &v }

func TestThrottleTreatsBoardCommitAsProgress(t *testing.T) {
	// A docs/PM lane's product IS the board. Judging it dry on code commits
	// alone would suspend the one lane class that never writes code — and the
	// pin could then only be cleared by work it is not supposed to do, i.e. a
	// permanent suspension dressed as a throttle.
	o := TickOutcome{Project: "docs-lane"}
	if !tickProducedWorkForThrottle(o, 0, 1, true) {
		t.Error("a board commit must count as progress for the throttle")
	}
	if tickProducedWorkForThrottle(o, 0, 0, true) {
		t.Error("no code commit, no board commit, no memory key = dry")
	}
	if !tickProducedWorkForThrottle(o, 1, 0, true) {
		t.Error("a code commit is progress")
	}
	// The pacing streak keeps the narrower SCHED-GAP-104 definition: board
	// churn must not make a lane look productive to the packer.
	if tickProducedWork(o, 0, true) {
		t.Error("board-only work must NOT count as progress for the pacing streak")
	}
}
