package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SCHED-GAP-1710 — the durable record of a tick that HANDED ITS WORK OUT over
// the Crier bus (migration 62, tick_dispatch).
//
// Motivation, stated once so the write side and the read side agree: a
// dispatched unit of work is not a process. There is no pid, no exit code, no
// stdout pipe and no gateway session — the tick's work runs somewhere else,
// under someone else's harness, and the only local facts are the agent that
// was addressed, the correlation id the answer must carry, the relay's
// message id for the hand-out, and (later) the answer itself. Forcing those
// into ticks' process columns would make session_id or exit_code lie, so they
// get their own per-tick row.
//
// WHAT THIS IS NOT. It is not the SCHED-GAP-1653 dispatch accountability pair
// on ticks (dispatch_outcome/dispatch_reason: "did this tick dispatch a
// foreman worker"). That vocabulary answers a different question about the
// local spawn path; this table answers "which named agent was this tick handed
// to, and did it answer".
//
// THE TWO LAWS enforced here:
//
//  1. The agent recorded is the one USED (from the relay's accept), never the
//     configured one — the configured-vs-used distinction is the whole point.
//  2. A reply is matched on the correlation id (or the relay message id it
//     answers), never by arrival order, so a foreign message on the same inbox
//     can never close the wrong tick.
const (
	// DispatchStateDispatched — the relay ACCEPTED the hand-out; the agent has
	// not answered yet.
	DispatchStateDispatched = "dispatched"
	// DispatchStateReplied — a correlated answer arrived and was recorded.
	DispatchStateReplied = "replied"
	// DispatchStateFailed — the hand-out itself failed (unknown target,
	// refusal, unreachable relay): the work was never queued, and there is no
	// silent fallback to any other transport.
	DispatchStateFailed = "failed"
	// DispatchStateExpired — the hand-out was accepted but no correlated answer
	// arrived inside the tick's reply window.
	DispatchStateExpired = "expired"
)

// DispatchStates is the closed vocabulary above, for validation and for a
// consumer that renders every bucket without inventing its own keys.
var DispatchStates = []string{
	DispatchStateDispatched, DispatchStateReplied, DispatchStateFailed, DispatchStateExpired,
}

// TickDispatch is one tick's hand-out record.
type TickDispatch struct {
	TickID    string
	Lane      string
	Agent     string // the agent ACTUALLY addressed (receipt), never the configured one
	CorrID    string
	MessageID string
	Transport string
	State     string
	IssuedAt  string
	UpdatedAt string
	Reply     string
	Error     string
}

// ErrDispatchReceiptFields names an incomplete receipt before any I/O: a
// hand-out record with no tick, lane, agent or correlation id cannot be
// correlated later, so it is refused at the decision site rather than stored
// as a row nothing can match.
func validateTickDispatch(r TickDispatch) error {
	switch {
	case r.TickID == "":
		return errors.New("record tick dispatch: tick id is required")
	case r.Lane == "":
		return errors.New("record tick dispatch: lane is required")
	case r.Agent == "":
		return errors.New("record tick dispatch: agent is required")
	case r.CorrID == "":
		return errors.New("record tick dispatch: corr id is required")
	}
	for _, s := range DispatchStates {
		if r.State == s {
			return nil
		}
	}
	return fmt.Errorf("record tick dispatch: state %q is outside the vocabulary %v", r.State, DispatchStates)
}

// RecordTickDispatchReceipt upserts the hand-out receipt for a tick. It is
// called ONCE per dispatch attempt (state=dispatched | failed) and once more
// when the answer arrives (state=replied | expired), so the row always
// describes the attempt's final known state.
func RecordTickDispatchReceipt(ctx context.Context, db *sql.DB, r TickDispatch) error {
	if err := validateTickDispatch(r); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO tick_dispatch
			(tick_id, lane, agent, corr_id, message_id, transport, state, issued_at, updated_at, reply, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tick_id) DO UPDATE SET
			agent=excluded.agent, corr_id=excluded.corr_id, message_id=excluded.message_id,
			transport=excluded.transport, state=excluded.state, updated_at=excluded.updated_at,
			reply=excluded.reply, error=excluded.error
	`, r.TickID, r.Lane, r.Agent, r.CorrID, r.MessageID, r.Transport, r.State, r.IssuedAt, r.UpdatedAt, r.Reply, r.Error)
	if err != nil {
		return fmt.Errorf("record tick dispatch %q: %w", r.TickID, err)
	}
	return nil
}

// FinishTickDispatch updates the state/reply of an EXISTING hand-out record
// (the reply leg's write). An unknown tick is ErrTickNotFound-like: the caller
// must never invent a receipt for work it did not dispatch.
func FinishTickDispatch(ctx context.Context, db *sql.DB, tickID, state, reply, errText, updatedAt string) error {
	if tickID == "" {
		return errors.New("finish tick dispatch: tick id is required")
	}
	ok := false
	for _, s := range DispatchStates {
		if s == state {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("finish tick dispatch: state %q is outside the vocabulary %v", state, DispatchStates)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE tick_dispatch SET state = ?, reply = ?, error = ?, updated_at = ? WHERE tick_id = ?
	`, state, reply, errText, updatedAt, tickID)
	if err != nil {
		return fmt.Errorf("finish tick dispatch %q: %w", tickID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish tick dispatch %q rows: %w", tickID, err)
	}
	if n == 0 {
		return fmt.Errorf("finish tick dispatch %q: %w", tickID, ErrTickNotFound)
	}
	return nil
}

// TickDispatchForTick reads one tick's hand-out record. ok=false means this
// tick dispatched nothing (a local tick) — the honest absence, not an error.
func TickDispatchForTick(ctx context.Context, db *sql.DB, tickID string) (TickDispatch, bool, error) {
	row := db.QueryRowContext(ctx, `
		SELECT tick_id, lane, agent, corr_id, message_id, transport, state, issued_at, updated_at, reply, error
		FROM tick_dispatch WHERE tick_id = ?
	`, tickID)
	return scanTickDispatch(row)
}

// TickDispatchByCorrID resolves an incoming answer to the tick that dispatched
// it, by the correlation id the answer carries. The lookup is the correlation
// law's implementation: the id names the tick, nothing else does.
func TickDispatchByCorrID(ctx context.Context, db *sql.DB, corrID string) (TickDispatch, bool, error) {
	if corrID == "" {
		return TickDispatch{}, false, nil
	}
	row := db.QueryRowContext(ctx, `
		SELECT tick_id, lane, agent, corr_id, message_id, transport, state, issued_at, updated_at, reply, error
		FROM tick_dispatch WHERE corr_id = ? ORDER BY updated_at DESC LIMIT 1
	`, corrID)
	return scanTickDispatch(row)
}

// TickDispatchByMessageID resolves an answer to the hand-out it answers using
// the relay's own message id (the fleet dispatcher echoes it as in_reply_to).
func TickDispatchByMessageID(ctx context.Context, db *sql.DB, messageID string) (TickDispatch, bool, error) {
	if messageID == "" {
		return TickDispatch{}, false, nil
	}
	row := db.QueryRowContext(ctx, `
		SELECT tick_id, lane, agent, corr_id, message_id, transport, state, issued_at, updated_at, reply, error
		FROM tick_dispatch WHERE message_id = ? ORDER BY updated_at DESC LIMIT 1
	`, messageID)
	return scanTickDispatch(row)
}

// scanTickDispatch reads one receipt row; no row is (zero, false, nil).
func scanTickDispatch(row *sql.Row) (TickDispatch, bool, error) {
	var r TickDispatch
	err := row.Scan(&r.TickID, &r.Lane, &r.Agent, &r.CorrID, &r.MessageID, &r.Transport,
		&r.State, &r.IssuedAt, &r.UpdatedAt, &r.Reply, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return TickDispatch{}, false, nil
	}
	if err != nil {
		return TickDispatch{}, false, fmt.Errorf("scan tick dispatch: %w", err)
	}
	return r, true, nil
}

// CountTickDispatchByState is the operator read: how many hand-outs are
// outstanding, answered, failed or expired for one lane ("" = every lane).
// Every vocabulary bucket is zero-filled so a consumer renders the full split
// without merging its own keys.
func CountTickDispatchByState(ctx context.Context, db *sql.DB, lane string) (map[string]int, error) {
	out := make(map[string]int, len(DispatchStates))
	for _, s := range DispatchStates {
		out[s] = 0
	}
	q := `SELECT state, COUNT(*) FROM tick_dispatch`
	args := []any{}
	if lane != "" {
		q += ` WHERE lane = ?`
		args = append(args, lane)
	}
	q += ` GROUP BY state`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return out, fmt.Errorf("count tick dispatch states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return out, fmt.Errorf("scan tick dispatch count: %w", err)
		}
		out[state] += n
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("iterate tick dispatch counts: %w", err)
	}
	return out, nil
}
