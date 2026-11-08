package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-17002 — wake tables.
//
// The property under test: a wake is a persisted promise that fires EXACTLY
// once, on its own terms (schedule or external), honours the two refusals
// that matter (blackout defers, busy lane skips), and never lies about what
// happened — the row and the events carry the attempt either way.

func newWakeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()
	p := database.Project{
		Name: "wake-lane", Workdir: t.TempDir(), Weight: 1, Priority: 5,
		CooldownS: 600, Enabled: true,
		CreatedAt: "2026-11-07T00:00:00Z", UpdatedAt: "2026-11-07T00:00:00Z",
	}
	if err := database.CreateProject(ctx, db, &p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return db
}

func wakeState(t *testing.T, db *sql.DB, id string) (string, sql.NullString) {
	t.Helper()
	var state, tick sql.NullString
	if err := db.QueryRow(`SELECT state, fired_tick_id FROM wakes WHERE id = ?`, id).Scan(&state, &tick); err != nil {
		t.Fatalf("read wake %s: %v", id, err)
	}
	return state.String, tick
}

func TestWakeTableDueFiresExactlyOnce(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()

	fires := 0
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		fires++
		return fmt.Sprintf("tick-%d", fires), nil
	})

	past := time.Now().Add(-time.Minute)
	id, err := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runner.pollOnce(ctx) // due → fires
	if fires != 1 {
		t.Fatalf("fires = %d after first pass, want 1", fires)
	}
	runner.pollOnce(ctx) // already claimed/fired → must NOT fire again
	if fires != 1 {
		t.Fatalf("fires = %d after second pass, want 1 (a wake fires EXACTLY once)", fires)
	}

	state, tick := wakeState(t, db, id)
	if state != database.WakeStateFired {
		t.Fatalf("state = %q, want fired", state)
	}
	if !tick.Valid || tick.String != "tick-1" {
		t.Fatalf("fired_tick_id = %v, want tick-1 (the row records what the promise produced)", tick)
	}
}

func TestWakeTableCancelledNeverFires(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	fires := 0
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		fires++
		return "t", nil
	})
	past := time.Now().Add(-time.Minute)
	id, _ := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, time.Now())

	if ok, err := database.CancelWake(ctx, db, id, "test"); err != nil || !ok {
		t.Fatalf("cancel: ok=%v err=%v", ok, err)
	}
	runner.pollOnce(ctx)
	if fires != 0 {
		t.Fatalf("a CANCELLED wake fired (%d) — the cancel is a lie", fires)
	}
	state, _ := wakeState(t, db, id)
	if state != database.WakeStateCancelled {
		t.Fatalf("state = %q, want cancelled", state)
	}
}

func TestWakeTableDailyRearmsForward(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	fires := 0
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		fires++
		return fmt.Sprintf("tick-%d", fires), nil
	})
	// Due NOW (today at HH:MM already passed is hard to construct portably —
	// use one minute ago via 'at' semantics on a daily's first occurrence).
	hhmm := time.Now().Add(-time.Minute).Format("15:04")
	first := time.Now().Add(-time.Minute)
	id, err := database.CreateWake(ctx, db, &database.Wake{
		Project: "wake-lane", Kind: database.WakeKindDaily, DailyHHMM: hhmm, AtTime: &first,
	}, time.Now())
	if err != nil {
		t.Fatalf("create daily: %v", err)
	}

	runner.pollOnce(ctx)
	if fires != 1 {
		t.Fatalf("fires = %d, want 1", fires)
	}
	state, _ := wakeState(t, db, id)
	if state != database.WakeStatePending {
		t.Fatalf("daily state = %q after firing, want pending (re-armed)", state)
	}
	// The re-armed at_time must be in the FUTURE — a daily that re-arms into
	// the past would fire in a tight loop.
	var at string
	if err := db.QueryRow(`SELECT at_time FROM wakes WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatalf("read at_time: %v", err)
	}
	atT, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatalf("re-armed at_time %q does not parse: %v", at, err)
	}
	if !atT.After(time.Now()) {
		t.Fatalf("re-armed at_time = %v, want in the future", atT)
	}
}

func TestWakeTableBusyLaneIsSkippedNotFailed(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		return "", ErrProjectRunning // the lane is mid-tick
	})
	past := time.Now().Add(-time.Minute)
	id, _ := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, time.Now())

	runner.pollOnce(ctx)
	state, _ := wakeState(t, db, id)
	if state != database.WakeStateSkipped {
		t.Fatalf("state = %q, want skipped (a busy lane is an attempt with no spawn, not an error)", state)
	}
}

func TestWakeTableBlackoutRepends(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		return "", ErrWakeBlackout
	})
	past := time.Now().Add(-time.Minute)
	id, _ := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, time.Now())

	runner.pollOnce(ctx)
	state, _ := wakeState(t, db, id)
	if state != database.WakeStatePending {
		t.Fatalf("state = %q, want pending — a blackout DEFERS the wake, it does not consume it", state)
	}
	var at string
	if err := db.QueryRow(`SELECT at_time FROM wakes WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatalf("read at_time: %v", err)
	}
	atT, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatalf("re-pended at_time %q does not parse: %v", at, err)
	}
	if !atT.After(time.Now()) {
		t.Fatalf("blackout re-pend must push the retry OUT (got at_time %v) — otherwise the runner hammer-polls the gate", atT)
	}
}

func TestWakeTableExternalFireOnlyWhenPending(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	fires := 0
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		fires++
		return "t-ext", nil
	})
	id, err := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindExternal}, time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A scheduled poll NEVER fires an external wake — it has no due time.
	runner.pollOnce(ctx)
	if fires != 0 {
		t.Fatalf("an external wake fired from the SCHEDULED pass (%d) — only /fire may trigger it", fires)
	}

	// The API fire path fires it exactly once.
	if _, ok, err := runner.FireExternal(ctx, id); err != nil || !ok {
		t.Fatalf("FireExternal: ok=%v err=%v", ok, err)
	}
	if fires != 1 {
		t.Fatalf("fires = %d after external fire, want 1", fires)
	}
	// Second fire: refused (409 at the API), no second spawn.
	if _, ok, err := runner.FireExternal(ctx, id); ok || err != nil {
		t.Fatalf("second FireExternal: ok=%v err=%v, want ok=false", ok, err)
	}
	if fires != 1 {
		t.Fatalf("fires = %d after the refused second fire, want 1 (once only)", fires)
	}
}

func TestWakeTableExternalRefusesScheduledKind(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		return "t", nil
	})
	future := time.Now().Add(time.Hour)
	id, _ := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &future}, time.Now())

	if _, ok, err := runner.FireExternal(ctx, id); ok || err == nil {
		t.Fatalf("FireExternal on kind=at: ok=%v err=%v, want refusal", ok, err)
	}
	// And the refusal must have put the wake BACK to pending.
	state, _ := wakeState(t, db, id)
	if state != database.WakeStatePending {
		t.Fatalf("state = %q after a refused external fire, want pending (the schedule still stands)", state)
	}
}

func TestWakeTableSpawnHookErrorIsSkipped(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()
	boom := errors.New("boom")
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		return "", boom
	})
	past := time.Now().Add(-time.Minute)
	id, _ := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, time.Now())

	runner.pollOnce(ctx)
	state, _ := wakeState(t, db, id)
	if state != database.WakeStateSkipped {
		t.Fatalf("state = %q, want skipped (the attempt is recorded, never hidden)", state)
	}
}

func TestWakeValidation(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := []struct {
		w    database.Wake
		want bool
		why  string
	}{
		{database.Wake{Project: "p", Kind: database.WakeKindAt, AtTime: &future}, true, ""},
		{database.Wake{Project: "p", Kind: database.WakeKindExternal}, true, ""},
		{database.Wake{Project: "p", Kind: database.WakeKindDaily, DailyHHMM: "02:00", AtTime: &future}, true, ""},
		{database.Wake{Project: "", Kind: database.WakeKindExternal}, false, "no project"},
		{database.Wake{Project: "p", Kind: "weird"}, false, "unknown kind"},
		{database.Wake{Project: "p", Kind: database.WakeKindAt}, false, "at without at_time"},
		{database.Wake{Project: "p", Kind: database.WakeKindAt, AtTime: &future, DailyHHMM: "02:00"}, false, "at + daily is a contradiction"},
		{database.Wake{Project: "p", Kind: database.WakeKindDaily, DailyHHMM: "25:00", AtTime: &future}, false, "invalid HH:MM"},
		{database.Wake{Project: "p", Kind: database.WakeKindDaily, DailyHHMM: "0200", AtTime: &future}, false, "malformed HH:MM"},
		{database.Wake{Project: "p", Kind: database.WakeKindExternal, AtTime: &future}, false, "external must not carry a time"},
	}
	for i, c := range cases {
		err := database.ValidateWakeRejects(&c.w)
		if c.want && err != nil {
			t.Errorf("case %d (%s): unexpected rejection: %v", i, c.why, err)
		}
		if !c.want && err == nil {
			t.Errorf("case %d (%s): accepted, want rejection", i, c.why)
		}
	}
}
