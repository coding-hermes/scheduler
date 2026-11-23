package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
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
	if state != database.WakeStatePending {
		t.Fatalf("state = %q, want pending (a busy lane is retried, not consumed)", state)
	}
}

/*
ch:trace row=REV-003 spec=none:board-row wave=none:worker-local test=internal/scheduler/wake_table_test.go::TestWakeTableBusyLaneRetriesAndThrottles doc=none:implementation-docs-if-needed prompt=none:foreman-dispatch-brief evidence=local-focused-tests witness=none:foreman-verifies verdict=none:pending-foreman commit=none:pending-worker memory=none:not-written
STATUS: complete
TIER: T1
*/
func TestWakeTableBusyLaneRetriesAndThrottles(t *testing.T) {
	db := newWakeTestDB(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	sim := clock.NewManualSimClock(now)
	defer sim.Close()
	ctx := clock.WithClock(context.Background(), sim)
	busyCalls := 0
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		busyCalls++
		return "", ErrProjectRunning
	})
	runner.SetClock(sim)

	first := now.Add(-time.Minute)
	id, err := database.CreateWake(ctx, db, &database.Wake{
		Project: "wake-lane", Kind: database.WakeKindDaily, DailyHHMM: "11:59", AtTime: &first,
	}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	for i := 0; i < 4; i++ {
		runner.pollOnce(ctx)
		sim.Advance(15 * time.Second)
	}
	if busyCalls != 1 {
		t.Fatalf("busy spawn attempts = %d in the 60-second window, want exactly 1 retry", busyCalls)
	}

	var retries int
	var retryAt string
	if err := db.QueryRow(`SELECT COUNT(*), at_time FROM wakes WHERE id = ? AND state = ?`, id, database.WakeStatePending).Scan(&retries, &retryAt); err != nil {
		t.Fatalf("read retried wake: %v", err)
	}
	if retries != 1 {
		t.Fatalf("pending wake rows = %d, want exactly 1 retrying wake", retries)
	}
	retryTime, err := time.Parse(time.RFC3339Nano, retryAt)
	if err != nil {
		t.Fatalf("retry at_time %q does not parse: %v", retryAt, err)
	}
	if !retryTime.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("retry at_time = %v, want %v", retryTime, now.Add(5*time.Minute))
	}

	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE component = 'wake_table' AND details LIKE ?`, "%"+id+"%").Scan(&events); err != nil {
		t.Fatalf("count wake events: %v", err)
	}
	if events > 2 {
		t.Fatalf("busy wake emitted %d events in the 60-second window, want at most 2", events)
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

// insertTickRow stands in for the production spawn's synchronous enqueue:
// the tick row exists the moment the spawn hook returns its id (loop.go —
// "the returned id is a real, stored row"), and the gateway stamps the
// terminal status moments later. outcome "" inserts NULL (a queued/running
// row has no outcome yet — the column's CHECK rejects non-terminal values).
func insertTickRow(t *testing.T, db *sql.DB, project, tickID, status, outcome, reason string, now time.Time) {
	t.Helper()
	var oc any
	if outcome != "" {
		oc = outcome
	}
	_, err := db.Exec(`INSERT INTO ticks (id, project_name, status, outcome, error, created_at)
		VALUES (?,?,?,?,?,?)`,
		tickID, project, status, oc, reason, now.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick row %s: %v", tickID, err)
	}
}

// wakeEventCensus reads every wake_table event and counts, per row: events
// claiming spawned=true (the settlement lie REV-004 forbids) and events
// carrying the MEDIUM override with the given reason text.
func wakeEventCensus(t *testing.T, db *sql.DB, reason string) (spawnedTrue, mediumReason int) {
	t.Helper()
	rows, err := db.Query(`SELECT details FROM events WHERE component = 'wake_table'`)
	if err != nil {
		t.Fatalf("read wake events: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var details string
		if err := rows.Scan(&details); err != nil {
			t.Fatalf("scan event details: %v", err)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(details), &d); err != nil {
			t.Fatalf("event details %q do not parse: %v", details, err)
		}
		if v, ok := d["spawned"].(bool); ok && v {
			spawnedTrue++
		}
		if sev, _ := d["severity_override"].(string); sev == string(SeverityMedium) && strings.Contains(details, reason) {
			mediumReason++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate wake events: %v", err)
	}
	return spawnedTrue, mediumReason
}

/*
ch:trace row=REV-004 spec=none:board-row wave=none:worker-local test=internal/scheduler/wake_table_test.go::TestWakeTableDeferredSpawnKeepsOccurrence doc=docs/wake-tables.md prompt=none:foreman-dispatch-brief evidence=local-focused-tests witness=none:foreman-verifies verdict=none:pending-foreman commit=none:pending-worker memory=none:not-written
STATUS: complete
TIER: T1
*/
func TestWakeTableDeferredSpawnKeepsOccurrence(t *testing.T) {
	db := newWakeTestDB(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	sim := clock.NewManualSimClock(now)
	defer sim.Close()
	ctx := clock.WithClock(context.Background(), sim)

	const tickID = "tick-deferred-1"
	const reason = "no gateway client and exec fallback disabled"
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		// The deferral has already landed by the time settleWake reads —
		// the observed live ordering under a gateway outage.
		insertTickRow(t, db, p.Name, tickID, string(database.StatusDeferred), "deferred", reason, now)
		return tickID, nil
	})
	runner.SetClock(sim)

	// A DAILY wake due one minute ago — exactly the shape whose occurrence
	// the old code consumed: settle-as-fired re-armed at_time to TOMORROW.
	first := now.Add(-time.Minute)
	id, err := database.CreateWake(ctx, db, &database.Wake{
		Project: "wake-lane", Kind: database.WakeKindDaily, DailyHHMM: first.Format("15:04"), AtTime: &first,
	}, now)
	if err != nil {
		t.Fatalf("create daily: %v", err)
	}

	runner.pollOnce(ctx)

	var state, at, firedAt, firedTick sql.NullString
	if err := db.QueryRow(`SELECT state, at_time, fired_at, fired_tick_id FROM wakes WHERE id = ?`, id).
		Scan(&state, &at, &firedAt, &firedTick); err != nil {
		t.Fatalf("read wake: %v", err)
	}
	if state.String != database.WakeStatePending {
		t.Fatalf("state = %q, want pending — a deferred spawn keeps the wake on its occurrence", state.String)
	}
	if firedAt.Valid || firedTick.Valid {
		t.Fatalf("fired_at=%v fired_tick_id=%v must stay NULL — the deferred tick never fired the wake", firedAt, firedTick)
	}
	atT, err := time.Parse(time.RFC3339Nano, at.String)
	if err != nil {
		t.Fatalf("at_time %q does not parse: %v", at.String, err)
	}
	if want := now.Add(5 * time.Minute); !atT.Equal(want) {
		t.Fatalf("at_time = %v, want %v (retry armed on the SAME occurrence, 5 minutes out)", atT, want)
	}
	nextOccurrence := time.Date(2026, 11, 23, first.Hour(), first.Minute(), 0, 0, time.UTC)
	if !atT.Before(nextOccurrence) {
		t.Fatalf("at_time = %v reached past the next occurrence %v — the day's occurrence was consumed", atT, nextOccurrence)
	}

	spawnedTrue, mediumReason := wakeEventCensus(t, db, reason)
	if spawnedTrue != 0 {
		t.Fatalf("%d wake event(s) claim spawned=true — a deferred spawn must never settle the wake as fired", spawnedTrue)
	}
	if mediumReason != 1 {
		t.Fatalf("MEDIUM events naming the deferral reason = %d, want exactly 1", mediumReason)
	}
	var mediumRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE component = 'wake_table' AND severity = ?`,
		string(SeverityMedium)).Scan(&mediumRows); err != nil {
		t.Fatalf("count MEDIUM events: %v", err)
	}
	if mediumRows != 1 {
		t.Fatalf("MEDIUM-severity wake events = %d, want exactly 1", mediumRows)
	}
}

func TestWakeTableDeferredOneShotRetriesNotSkips(t *testing.T) {
	db := newWakeTestDB(t)
	now := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	sim := clock.NewManualSimClock(now)
	defer sim.Close()
	ctx := clock.WithClock(context.Background(), sim)

	const tickID = "tick-deferred-2"
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		insertTickRow(t, db, p.Name, tickID, string(database.StatusFailed), "failed", "gateway connect refused", now)
		return tickID, nil
	})
	runner.SetClock(sim)

	past := now.Add(-time.Minute)
	id, err := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	runner.pollOnce(ctx)

	state, tick := wakeState(t, db, id)
	if state != database.WakeStatePending {
		t.Fatalf("state = %q, want pending — a one-shot whose tick failed retries too (not skipped, not fired)", state)
	}
	if tick.Valid {
		t.Fatalf("fired_tick_id = %v must stay NULL for a not-fired tick", tick)
	}
	var at string
	if err := db.QueryRow(`SELECT at_time FROM wakes WHERE id = ?`, id).Scan(&at); err != nil {
		t.Fatalf("read at_time: %v", err)
	}
	atT, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		t.Fatalf("at_time %q does not parse: %v", at, err)
	}
	if want := now.Add(5 * time.Minute); !atT.Equal(want) {
		t.Fatalf("at_time = %v, want %v (the retry is pushed 5 minutes out)", atT, want)
	}
	spawnedTrue, _ := wakeEventCensus(t, db, "gateway connect refused")
	if spawnedTrue != 0 {
		t.Fatalf("%d wake event(s) claim spawned=true for a failed tick", spawnedTrue)
	}
}

func TestWakeTableRunningTickSettlesFiredToday(t *testing.T) {
	db := newWakeTestDB(t)
	ctx := context.Background()

	const tickID = "tick-still-running"
	runner := NewWakeTableRunner(db, func(p database.Project, wakeID string) (string, error) {
		insertTickRow(t, db, p.Name, tickID, string(database.StatusRunning), "", "", time.Now())
		return tickID, nil
	})
	past := time.Now().Add(-time.Minute)
	id, err := database.CreateWake(ctx, db, &database.Wake{Project: "wake-lane", Kind: database.WakeKindAt, AtTime: &past}, time.Now())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The tick never leaves 'running' inside the observation window: the
	// wake must settle exactly as today — fired, tick recorded, spawned=true
	// event emitted. This guards the live production seam.
	runner.pollOnce(ctx)
	state, tick := wakeState(t, db, id)
	if state != database.WakeStateFired {
		t.Fatalf("state = %q, want fired — a tick still in flight settles as fired (happy path unchanged)", state)
	}
	if !tick.Valid || tick.String != tickID {
		t.Fatalf("fired_tick_id = %v, want %s", tick, tickID)
	}
	spawnedTrue, _ := wakeEventCensus(t, db, "")
	if spawnedTrue != 1 {
		t.Fatalf("spawned=true wake events = %d, want exactly 1", spawnedTrue)
	}
}
