package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SCHED-GAP-17002 — WAKE TABLES.
//
// The owner's ask: "support for wake tables — if i say to have a set time or
// event in the future for waking up and running a lane they can be programmed
// via api etc."
//
// A wake table entry is a PERSISTED promise: run this lane at that time (or
// when this external event says so). It lives in scheduler.db because it is
// runtime scheduling state, not configuration — the same class as ticks. The
// row is never deleted: cancel is a state, fire is a state, and the record is
// the audit of who asked for what and what came of it.

// Wake states.
const (
	WakeStatePending   = "pending"
	WakeStateFiring    = "firing" // claimed by a runner, spawn in flight
	WakeStateFired     = "fired"
	WakeStateCancelled = "cancelled"
	WakeStateSkipped   = "skipped" // lane was busy / blackout deferred re-pends instead
)

// Wake kinds.
const (
	WakeKindAt       = "at"       // one-shot at an absolute RFC3339 instant
	WakeKindDaily    = "daily"    // recurring at a local wall-clock HH:MM
	WakeKindExternal = "external" // fires only when POSTed to /fire
)

// Wake is one row of the wake table.
type Wake struct {
	ID          string     `json:"id"`
	Project     string     `json:"project"`
	Kind        string     `json:"kind"`
	AtTime      *time.Time `json:"at_time"`    // kind=at (and the NEXT fire for daily)
	DailyHHMM   string     `json:"daily_hhmm"` // kind=daily, local wall clock "HH:MM"
	Payload     string     `json:"payload"`    // opaque; carried onto the event + log
	State       string     `json:"state"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   string     `json:"created_at"`
	FiredAt     *string    `json:"fired_at"`
	FiredTickID *string    `json:"fired_tick_id"`
	Note        string     `json:"note"`
}

// ValidateWakeRejects mirrors the API's duty at the storage layer: exactly one
// scheduling clause per kind, times parse, and the identity fields are present.
func ValidateWakeRejects(w *Wake) error {
	if w.Project == "" {
		return fmt.Errorf("wake: project is required")
	}
	switch w.Kind {
	case WakeKindAt:
		if w.AtTime == nil {
			return fmt.Errorf("wake kind 'at' requires at_time (RFC3339)")
		}
		if w.DailyHHMM != "" {
			return fmt.Errorf("wake kind 'at' must not set daily_hhmm")
		}
	case WakeKindDaily:
		if len(w.DailyHHMM) != 5 || w.DailyHHMM[2] != ':' {
			return fmt.Errorf("wake kind 'daily' requires daily_hhmm as local \"HH:MM\"")
		}
		var h, m int
		if _, err := fmt.Sscanf(w.DailyHHMM, "%02d:%02d", &h, &m); err != nil || h > 23 || m > 59 {
			return fmt.Errorf("wake kind 'daily': %q is not a valid local HH:MM", w.DailyHHMM)
		}
		if w.AtTime == nil {
			return fmt.Errorf("wake kind 'daily' requires at_time = its FIRST occurrence")
		}
	case WakeKindExternal:
		if w.AtTime != nil || w.DailyHHMM != "" {
			return fmt.Errorf("wake kind 'external' is fired via API and must not set at_time/daily_hhmm")
		}
	default:
		return fmt.Errorf("wake kind must be one of at|daily|external, got %q", w.Kind)
	}
	return nil
}

// CreateWake inserts a pending wake. The id is assigned here (wk-<unixnano
// hex>) so callers can create without a round trip. now is the caller's clock
// instant — the database package owns no clock seam, so every timestamp is
// supplied (the same discipline the rest of the package keeps).
func CreateWake(ctx context.Context, db *sql.DB, w *Wake, now time.Time) (string, error) {
	if err := ValidateWakeRejects(w); err != nil {
		return "", err
	}
	if w.ID == "" {
		w.ID = fmt.Sprintf("wk-%x", now.UnixNano())
	}
	if w.State == "" {
		w.State = WakeStatePending
	}
	if w.CreatedAt == "" {
		w.CreatedAt = now.UTC().Format(time.RFC3339)
	}
	var at any
	if w.AtTime != nil {
		at = w.AtTime.UTC().Format(time.RFC3339Nano)
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO wakes (id, project, kind, at_time, daily_hhmm, payload, state, created_by, created_at, note)
VALUES (?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Project, w.Kind, at, w.DailyHHMM, w.Payload, w.State, w.CreatedBy, w.CreatedAt, w.Note)
	if err != nil {
		return "", fmt.Errorf("create wake: %w", err)
	}
	return w.ID, nil
}

const wakeColumns = `id, project, kind, at_time, daily_hhmm, payload, state, created_by, created_at, fired_at, fired_tick_id, note`

func scanWake(row interface{ Scan(...any) error }) (*Wake, error) {
	var w Wake
	var at, firedAt, firedTick sql.NullString
	if err := row.Scan(&w.ID, &w.Project, &w.Kind, &at, &w.DailyHHMM, &w.Payload, &w.State,
		&w.CreatedBy, &w.CreatedAt, &firedAt, &firedTick, &w.Note); err != nil {
		return nil, err
	}
	if at.Valid {
		if t, err := time.Parse(time.RFC3339Nano, at.String); err == nil {
			w.AtTime = &t
		}
	}
	if firedAt.Valid {
		w.FiredAt = &firedAt.String
	}
	if firedTick.Valid {
		w.FiredTickID = &firedTick.String
	}
	return &w, nil
}

// ListWakes returns wakes newest-first, optionally filtered by state.
func ListWakes(ctx context.Context, db *sql.DB, state string) ([]*Wake, error) {
	q := `SELECT ` + wakeColumns + ` FROM wakes`
	var args []any
	if state != "" {
		q += ` WHERE state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Wake{}
	for rows.Next() {
		w, err := scanWake(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWake returns one wake or sql.ErrNoRows.
func GetWake(ctx context.Context, db *sql.DB, id string) (*Wake, error) {
	return scanWake(db.QueryRowContext(ctx, `SELECT `+wakeColumns+` FROM wakes WHERE id = ?`, id))
}

// ClaimDueWakes atomically claims every pending, due wake: state pending →
// firing, returning the claimed rows. The conditional UPDATE is the claim —
// two runners (or a runner and an API fire) can never both fire the same wake.
func ClaimDueWakes(ctx context.Context, db *sql.DB, now time.Time) ([]*Wake, error) {
	// Collect due ids first (cheap, indexed), then claim each one-by-one so
	// the claim returns exactly the rows THIS caller won.
	ids := []string{}
	rows, err := db.QueryContext(ctx, `
SELECT id FROM wakes
 WHERE state = ? AND kind IN (?, ?) AND at_time IS NOT NULL AND at_time <= ?`,
		WakeStatePending, WakeKindAt, WakeKindDaily, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	claimed := []*Wake{}
	for _, id := range ids {
		res, err := db.ExecContext(ctx, `
UPDATE wakes SET state = ? WHERE id = ? AND state = ?`, WakeStateFiring, id, WakeStatePending)
		if err != nil {
			return claimed, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // someone else claimed it — exactly the point of the gate
		}
		if w, err := GetWake(ctx, db, id); err == nil {
			claimed = append(claimed, w)
		}
	}
	return claimed, nil
}

// ClaimWake claims ONE wake by id regardless of due time — the external-event
// fire path. Refuses (ok=false) anything not pending.
func ClaimWake(ctx context.Context, db *sql.DB, id string) (*Wake, bool, error) {
	res, err := db.ExecContext(ctx, `
UPDATE wakes SET state = ? WHERE id = ? AND state = ?`, WakeStateFiring, id, WakeStatePending)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false, nil
	}
	w, err := GetWake(ctx, db, id)
	if err != nil {
		return nil, true, err
	}
	return w, true, nil
}

// CancelWake marks a pending wake cancelled (never deletes — the record is the
// audit). Returns ok=false when the wake was not in pending state.
func CancelWake(ctx context.Context, db *sql.DB, id, by string) (bool, error) {
	res, err := db.ExecContext(ctx, `
UPDATE wakes SET state = ?, note = COALESCE(NULLIF(?,''), note)
 WHERE id = ? AND state = ?`, WakeStateCancelled, "cancelled by "+by, id, WakeStatePending)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SettleFiredWake finalizes a claimed wake after its spawn attempt:
// state=fired with the produced tick id, or — for a daily — re-armed to its
// next occurrence (state back to pending). A lane-busy skip leaves the row
// SKIPPED for one-shots (the attempt is visible) and re-pends dailies.
func SettleFiredWake(ctx context.Context, db *sql.DB, w *Wake, outcome WakeSettlement, now time.Time) error {
	stamped := now.UTC().Format(time.RFC3339)
	switch {
	case outcome.Spawned && w.Kind == WakeKindDaily:
		next := nextDailyAfter(w.DailyHHMM, now)
		_, err := db.ExecContext(ctx, `
UPDATE wakes SET state = ?, fired_at = COALESCE(fired_at, ?), fired_tick_id = ?, at_time = ? WHERE id = ?`,
			WakeStatePending, stamped, outcome.TickID, next.UTC().Format(time.RFC3339Nano), w.ID)
		return err
	case outcome.Spawned:
		_, err := db.ExecContext(ctx, `
UPDATE wakes SET state = ?, fired_at = ?, fired_tick_id = ? WHERE id = ?`,
			WakeStateFired, stamped, outcome.TickID, w.ID)
		return err
	case w.Kind == WakeKindDaily:
		// A skipped daily keeps its schedule: back to pending at the SAME
		// occurrence (the runner will retry after the lane frees).
		_, err := db.ExecContext(ctx, `UPDATE wakes SET state = ? WHERE id = ?`, WakeStatePending, w.ID)
		return err
	default:
		_, err := db.ExecContext(ctx, `UPDATE wakes SET state = ? WHERE id = ?`, WakeStateSkipped, w.ID)
		return err
	}
}

// WakeSettlement is the spawn hook's answer for one fired wake.
type WakeSettlement struct {
	Spawned bool
	TickID  string
}

// nextDailyAfter returns the NEXT occurrence of a local HH:MM strictly after
// now (today's time if still ahead, else tomorrow).
func nextDailyAfter(hhmm string, now time.Time) time.Time {
	var h, m int
	if n, _ := fmt.Sscanf(hhmm, "%02d:%02d", &h, &m); n != 2 {
		// Unparseable (a state only a corrupted row could reach — the API
		// and CreateWake both validate): treat as "now" so the runner's
		// next poll re-arms the attempt instead of arming the past.
		return now
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// PushWakeRetry re-pends a claimed wake with its next attempt pushed out by
// wait (the blackout path): the wake keeps its promise, the runner just stops
// hammering the gate every poll. Only a firing wake can be pushed.
func PushWakeRetry(ctx context.Context, db *sql.DB, id string, wait time.Duration, now time.Time) error {
	next := now.UTC().Add(wait).Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
UPDATE wakes SET state = ?, at_time = ? WHERE id = ? AND state = ?`,
		WakeStatePending, next, id, WakeStateFiring)
	return err
}
