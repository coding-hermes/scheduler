package scheduler

// SCHED-GAP-1710 — DISPATCH FROM THE TICK PATH: a tick hands one unit of work
// to one named agent over the Crier bus.
//
// The bus client has spoken three verbs since REMOTE-004/009/1665 — Publish
// (announce), Query (ask a peer), Dispatch (hand work to an agent) — but the
// tick path only ever called the first two: the ONLY caller of Dispatch in the
// tree was cmd/scheduler-dispatch, a standalone operator CLI. So the scheduler
// could talk about work and never hand it over, which is the missing leg of a
// fleet designed as one agent per project.
//
// WHAT A TICK NOW DOES, for a lane that declares an execution target
// (internal/scheduler/dispatch_targets.go):
//
//  1. it hands the agent ONE unit of work carrying the lane, a board/workdir
//     REFERENCE the agent resolves to its own checkout, and a correlation id —
//     never a task id (the scheduler picks the LANE, the foreman picks the
//     TASK, dispatch-spec.md §3);
//  2. it records the RECEIPT against the tick: the agent actually addressed,
//     the correlation id, the relay's message id, where the relay put it
//     (database.tick_dispatch, migration 62);
//  3. it waits for the ANSWER on its own durable inbox and correlates it back
//     to THIS tick — by the relay message id the answer replies to, or by the
//     correlation id it echoes — recording the answer and completing the tick
//     with the agent's own verdict.
//
// THE THREE LAWS THIS FILE KEEPS:
//
//	NO TASK ID. The payload is lane + reference + correlation id. Anything
//	that named a task would move task selection into the scheduler.
//
//	NO FALLBACK. An unreachable, unknown or refusing target is a NAMED error
//	and the tick FAILS; it is never quietly re-pointed at the shared gateway.
//	A dispatch is what the scheduler decided to schedule, so its failure has
//	to be loud (dispatch-spec.md §2 — the deliberate inversion of the
//	autonomy law, scoped to this path only).
//
//	CORRELATION BY IDENTITY, NEVER BY ARRIVAL. A message on the scheduler's
//	inbox that cannot be matched to a hand-out this process made is left
//	strictly alone: not read for another owner's benefit, not acked, and never
//	allowed to close a tick. Only a message whose correlation id (or the relay
//	message id it answers) names a dispatch WE made is acked.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// Environment that arms the RECEIVE half of the leg: the scheduler's own bus
// inbox. The relay's inbox routes are agent-scoped and require the mailbox
// owner's ed25519 signature, so both are needed. An identity is REQUIRED for a
// remote lane: a hand-out whose answer can never be correlated is exactly the
// silent-failure shape this feature exists to remove, so the leg refuses to
// dispatch rather than dispatch blind.
const (
	// EnvDispatchAgentID is the agent id whose inbox the scheduler reads
	// (it must be registered on the relay with the key below).
	EnvDispatchAgentID = "CRIER_AGENT_ID"
	// EnvDispatchAgentKeyFile is the PKCS#8 PEM ed25519 key `crier keygen`
	// wrote for that agent id.
	EnvDispatchAgentKeyFile = "CRIER_AGENT_KEY_FILE"
)

// Polling shape of the reply leg. The lease is deliberately SHORT relative to
// the poll interval so a message this consumer is not the owner of returns to
// its rightful owner quickly, and long enough that the matching message is not
// stolen from us mid-read.
const (
	dispatchReplyPollInterval = 10 * time.Second
	dispatchReplyLeaseSeconds = 45
	dispatchReplyBatchLimit   = 25
)

// DispatchedTickTrigger labels how the tick ran in the delivered report.
const DispatchedTickTrigger = "dispatch"

// Errors from the dispatch leg. Named so a caller can branch, and so the tick's
// error column carries the mechanism rather than wire noise.
var (
	// ErrDispatchNotConfigured — the lane names an agent but the scheduler
	// cannot dispatch (no bus client, or no reply identity) so NOTHING was
	// attempted. Never a silent local run.
	ErrDispatchNotConfigured = errors.New("scheduler: remote lane cannot dispatch")
)

// SetDispatchIdentityFromEnv arms the bus client's inbox identity from the
// environment (CRIER_AGENT_ID + CRIER_AGENT_KEY_FILE). It is called once at
// boot; a missing or unreadable identity is logged and leaves the receive leg
// unarmed — a lane that then declares a remote target fails loudly at dispatch
// time instead of running blind.
func SetDispatchIdentityFromEnv(client *bus.Client) error {
	if client == nil {
		return nil
	}
	id := strings.TrimSpace(getEnvOrDefault(EnvDispatchAgentID, ""))
	keyPath := strings.TrimSpace(getEnvOrDefault(EnvDispatchAgentKeyFile, ""))
	if id == "" && keyPath == "" {
		return nil
	}
	if id == "" || keyPath == "" {
		return fmt.Errorf("%s and %s must both be set to arm the scheduler's inbox (got id=%q key=%q)",
			EnvDispatchAgentID, EnvDispatchAgentKeyFile, id, keyPath)
	}
	key, err := bus.LoadAgentKeyFile(keyPath)
	if err != nil {
		return err
	}
	client.SetAgentIdentity(id, key)
	return nil
}

// busClientForDispatch returns the process's bus client, or nil when the bus
// is disabled/unset (no loop, no bus, or a disabled client).
func (p *SlotPool) busClientForDispatch() *bus.Client {
	loop := p.loopOrNil()
	if loop == nil {
		return nil
	}
	return loop.Bus().Client()
}

// dispatchRemote performs the whole remote leg for one tick: hand out, record,
// wait for the correlated answer, record. It returns a SpawnedTick whose
// Wait() yields the tick's outcome (so slot_pool's completion, delivery and
// accounting tail is reused unchanged), or an error when the hand-out itself
// could not be made — in which case the caller MUST fail the tick and MUST NOT
// fall back to a local spawn.
func (p *SlotPool) dispatchRemote(ctx context.Context, proj PackedProject, tickID string, target DispatchTarget, db *sql.DB) (*SpawnedTick, error) {
	client := p.busClientForDispatch()
	if client == nil || !client.Enabled() {
		return nil, fmt.Errorf("%w: lane %s is configured for agent %q but the scheduler's bus client is disabled (add [crier] and the bus env, then restart)",
			ErrDispatchNotConfigured, proj.Name, target.Agent)
	}
	if !client.InboxEnabled() {
		return nil, fmt.Errorf("%w: lane %s is configured for agent %q but the scheduler has no inbox identity (%s + %s) — refusing to hand out work whose answer could never be correlated",
			ErrDispatchNotConfigured, proj.Name, target.Agent, EnvDispatchAgentID, EnvDispatchAgentKeyFile)
	}

	board, workdir := resolveDispatchReferences(target, proj)
	issued := p.clock().Now()

	// The wait is cancellable through the SAME session-cancel registry every
	// local tick registers with, so the one abort mechanism that exists (the
	// builder no-artifact guard) can end this wait instead of leaving a poller
	// parked; the entry is dropped on every return path. (A drain does not
	// cancel sessions — it waits out `stopGrace` and marks the row, exactly as
	// it does for a local tick.)
	dctx, dcancel := context.WithCancel(ctx)
	defer dcancel()
	p.spawner.RegisterTickSessionContext(tickID, dcancel)
	defer p.spawner.UnregisterTickSessionContext(tickID)

	item := bus.WorkItem{
		Lane:    proj.Name,
		Board:   board,
		Workdir: workdir,
		CorrID:  client.NextCorrID(),
		// The scheduler's OWN inbox: the proven fleet dispatcher answers
		// payload.reply_to, so naming ourselves is what makes the reply
		// arrive where the correlation id can be matched.
		ReplyTo: client.InboxAgentID(),
	}

	receipt, err := client.Dispatch(dctx, target.Agent, item)
	if err != nil {
		// LOUD, and never a fallback: the work was NOT handed out.
		attempt := database.TickDispatch{
			TickID: tickID, Lane: proj.Name,
			Agent:  target.Agent, // the address ATTEMPTED (no receipt exists)
			CorrID: item.CorrID, State: database.DispatchStateFailed,
			IssuedAt: issued.UTC().Format(time.RFC3339Nano), UpdatedAt: p.clock().Now().UTC().Format(time.RFC3339Nano),
			Error: err.Error(),
		}
		p.recordDispatchReceipt(db, attempt)
		log.Printf("DISPATCH: %s tick=%s agent=%s corr=%s REFUSED: %v", proj.Name, tickID, target.Agent, item.CorrID, err)
		return nil, err
	}
	// The recorded agent is the one USED (the relay's accept), never the
	// configured one — the configured-vs-used distinction is the point.
	rec := database.TickDispatch{
		TickID: tickID, Lane: proj.Name, Agent: receipt.AgentID,
		CorrID: receipt.CorrID, MessageID: receipt.MessageID, Transport: receipt.Transport,
		State:    database.DispatchStateDispatched,
		IssuedAt: issued.UTC().Format(time.RFC3339Nano),
	}
	p.recordDispatchReceipt(db, rec)
	log.Printf("DISPATCH: %s tick=%s agent=%s corr=%s message=%s transport=%s lane_ref=%s board_ref=%s",
		proj.Name, tickID, receipt.AgentID, receipt.CorrID, receipt.MessageID, receipt.Transport, workdir, board)

	// Wait for the answer, bounded by the same session deadline a local tick
	// would have had.
	timeout := p.spawner.effectiveTickTimeout(proj)
	reply, ok, waitErr := p.awaitDispatchReply(dctx, proj, tickID, receipt, timeout)

	finished := p.clock().Now()
	rec.UpdatedAt = finished.UTC().Format(time.RFC3339Nano)
	rec.Reply = reply
	st := &SpawnedTick{
		TickID:      tickID,
		Project:     proj.Name,
		Started:     issued,
		Deliver:     proj.Deliver,
		DeliverMode: proj.DeliverMode,
		Trigger:     DispatchedTickTrigger,
		spawner:     p.spawner,
		tickTimeout: timeout,
	}
	if reply != "" {
		st.Output.WriteString(reply)
	}
	switch {
	case waitErr != nil:
		rec.State = database.DispatchStateExpired
		rec.Error = waitErr.Error()
		// SCHED-GAP-1707: the hand-out ran but its reply never arrived —
		// the row's telemetry (none: the agent's usage never reached this
		// process) is PARTIAL by the dispatch deadline, named
		// dispatch_deadline, with the quiet wall on session_silence_s.
		out := TickOutcome{
			TickID: tickID, Project: proj.Name, SessionID: receipt.CorrID,
			Started: issued, Finished: finished, Duration: finished.Sub(issued),
			Status: TickTimeout, ExitCode: -1,
			Error:      waitErr.Error(),
			CostSource: CostSourceGateway,
			// The hand-out RAN (the relay accepted it), so the dispatch
			// accountability pair says so; the tick merely ran out of wall.
			DispatchDispatched: true, DispatchReason: database.DispatchReasonDispatched,
		}
		out.TelemetryPartial = true
		out.TelemetryPartialReason = TelemetryPartialDispatchDeadline
		out.TelemetrySilenceS = int64(out.Duration / time.Second)
		st.remoteOutcome = &out
	case !ok:
		rec.State = database.DispatchStateReplied
		rec.Error = reply
		st.remoteOutcome = &TickOutcome{
			TickID: tickID, Project: proj.Name, SessionID: receipt.CorrID,
			Started: issued, Finished: finished, Duration: finished.Sub(issued),
			Status: TickFailed, ExitCode: -1, Error: reply,
			CostSource:         CostSourceGateway,
			DispatchDispatched: true, DispatchReason: database.DispatchReasonDispatched,
		}
	default:
		rec.State = database.DispatchStateReplied
		st.remoteOutcome = &TickOutcome{
			TickID: tickID, Project: proj.Name, SessionID: receipt.CorrID,
			Started: issued, Finished: finished, Duration: finished.Sub(issued),
			Status:             TickCompleted,
			CostSource:         CostSourceGateway,
			DispatchDispatched: true, DispatchReason: database.DispatchReasonDispatched,
		}
	}
	p.recordDispatchReceipt(db, rec)
	return st, nil
}

// awaitDispatchReply polls the scheduler's own inbox until a message
// correlating to THIS hand-out arrives, the deadline passes, or the caller's
// context ends. Messages that do not correlate are never acked: they belong to
// someone else, and the short lease returns them to their owner.
//
// Reply correlation accepts the two identities the hand-out actually
// established — the relay message id (echoed by the fleet dispatcher as
// in_reply_to) and the correlation id (echoed directly, or inside the original
// payload the dispatcher hands back as `task`) — and nothing else.
func (p *SlotPool) awaitDispatchReply(ctx context.Context, proj PackedProject, tickID string, receipt bus.DispatchReceipt, budget time.Duration) (string, bool, error) {
	client := p.busClientForDispatch()
	if client == nil || !client.InboxEnabled() {
		return "", false, fmt.Errorf("%w: no inbox identity while awaiting the answer to %s", ErrDispatchNotConfigured, receipt.CorrID)
	}
	if budget <= 0 {
		budget = DefaultGatewayResponseTimeout
	}
	deadline := p.clock().Now().Add(budget)
	for {
		lease, err := client.InboxRetrieve(ctx, dispatchReplyBatchLimit, dispatchReplyLeaseSeconds)
		if err != nil {
			// A bus blip is not a verdict about the work: log and keep
			// waiting until the budget runs out.
			log.Printf("DISPATCH: %s tick=%s inbox retrieve: %v (retrying)", proj.Name, tickID, err)
		} else {
			for _, msg := range lease.Messages {
				payload, matched := correlateDispatchReply(msg, receipt)
				if !matched {
					continue
				}
				if aerr := client.InboxAck(ctx, lease.LeaseID, msg.ID); aerr != nil {
					log.Printf("DISPATCH: %s tick=%s ack %s: %v", proj.Name, tickID, msg.ID, aerr)
				}
				ok, why := dispatchReplyVerdict(payload)
				text := dispatchReplyText(payload)
				if !ok {
					if strings.TrimSpace(text) == "" {
						text = why
					}
					return text, false, nil
				}
				return text, true, nil
			}
		}
		if !p.clock().Now().Before(deadline) {
			return "", false, fmt.Errorf("no correlated reply for corr=%s (message=%s) within %s — the hand-out is recorded and the agent may still answer",
				receipt.CorrID, receipt.MessageID, budget.Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return "", false, fmt.Errorf("dispatch %s: %w", receipt.CorrID, ctx.Err())
		case <-p.clock().After(dispatchReplyPollInterval):
		}
	}
}

// correlateDispatchReply decides whether one inbox message is the answer to a
// hand-out this scheduler made. It returns the payload and whether it matched.
func correlateDispatchReply(msg bus.InboxMessage, receipt bus.DispatchReceipt) (map[string]any, bool) {
	payload, err := msg.Payload()
	if err != nil {
		// A payload we cannot read is not an answer we may act on.
		return nil, false
	}
	if s, _ := payload["in_reply_to"].(string); s != "" && receipt.MessageID != "" && s == receipt.MessageID {
		return payload, true
	}
	if s, _ := payload["corr_id"].(string); s != "" && s == receipt.CorrID {
		return payload, true
	}
	// The proven fleet dispatcher echoes the ORIGINAL payload back inside
	// `task` so a consumer can see what was asked. Two shapes exist in the
	// wild: the dispatcher's own renderer emits it as a STRING, and a stricter
	// agent emits it as an OBJECT. BOTH are matched (the relay's content guard
	// blocks stringified-JSON payloads outright — measured 2026-10-03,
	// GUARD_BLOCKED "deterministic prematch block: stringified_json" — so the
	// structured shape is the one that survives a guard outage; the primary
	// correlation, `in_reply_to`, needs neither).
	if echoed, ok := payload["task"].(map[string]any); ok {
		if id, _ := echoed["corr_id"].(string); id == receipt.CorrID {
			return payload, true
		}
	}
	if s, ok := payload["task"].(string); ok && strings.Contains(s, receipt.CorrID) {
		var echoed map[string]any
		if json.Unmarshal([]byte(s), &echoed) == nil {
			if id, _ := echoed["corr_id"].(string); id == receipt.CorrID {
				return payload, true
			}
		}
	}
	return nil, false
}

// dispatchReplyVerdict reads the answering agent's own verdict when it sent
// one. Absence of a verdict is a SUCCESS (the agent answered): the scheduler
// has no basis to call an answer a failure just because it did not carry a
// flag.
func dispatchReplyVerdict(payload map[string]any) (bool, string) {
	if v, ok := payload["ok"].(bool); ok && !v {
		for _, k := range []string{"error", "err", "reply", "text", "message"} {
			if s, ok := payload[k].(string); ok && strings.TrimSpace(s) != "" {
				return false, s
			}
		}
		return false, "the agent reported ok=false"
	}
	return true, ""
}

// dispatchReplyText extracts the agent's answer text. An answer that is not a
// string is rendered as JSON rather than dropped: an opaque reply is still a
// reply, and the operator can read it.
func dispatchReplyText(payload map[string]any) string {
	for _, k := range []string{"reply", "answer", "text", "message", "output"} {
		if s, ok := payload[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(raw)
}

// recordDispatchReceipt persists one receipt write. Best-effort by contract in
// the sense that it can never change the tick's outcome, but NEVER silent: a
// failed write is logged with the tick, because the record is the audit trail
// the scheduled work is judged by.
func (p *SlotPool) recordDispatchReceipt(db *sql.DB, rec database.TickDispatch) {
	if db == nil {
		return
	}
	if rec.UpdatedAt == "" {
		rec.UpdatedAt = p.clock().Now().UTC().Format(time.RFC3339Nano)
	}
	if err := database.RecordTickDispatchReceipt(context.Background(), db, rec); err != nil {
		log.Printf("DISPATCH: %s tick=%s receipt write failed: %v", rec.Lane, rec.TickID, err)
	}
}
