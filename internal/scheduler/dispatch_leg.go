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
//	inbox that cannot be matched to a hand-out this process made is never
//	allowed to close a tick. SCHED-GAP-16984 amended the silence: such a
//	message is now COUNTED and NAMED — one structured log line per retrieve
//	batch, the same numbers and the correlation keys it claimed in the tick's
//	dispatch receipt, and per-process counters on /api/v1/metrics — and then
//	settled: true residue is acked (five real cf-node answers once parked
//	unacked to TTL because "answered with the wrong key" looked identical to
//	"never answered"), while a message whose claimed identity names another
//	tick's OUTSTANDING hand-out is deferred with that reason, never consumed,
//	so a concurrent await can still collect its own answer.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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

// dispatchToTarget performs the hand-out itself, honouring a REGISTERED agent's
// declared relay endpoint for an EXPLICITLY targeted dispatch (SCHED-GAP-1665).
//
// Two rules keep this additive:
//
//   - only an explicit per-tick override consults the registry. A lane whose
//     target file names the agent keeps riding the scheduler's configured
//     relay, byte-identically to the pre-registry behaviour.
//   - an unregistered agent, a registered agent with an empty endpoint, or any
//     registry read failure all leave baseURL empty → the configured relay. The
//     registry can never make a dispatch that works today fail.
//
// A non-empty endpoint is logged, because it moves the request to a relay the
// operator must have prepared (and whose answer must reach this scheduler's
// inbox — see bus.Client.DispatchTo).
func dispatchToTarget(ctx context.Context, client *bus.Client, proj PackedProject, target DispatchTarget, item bus.WorkItem, db *sql.DB) (bus.DispatchReceipt, error) {
	baseURL := ""
	if strings.TrimSpace(proj.DispatchAgent) != "" && db != nil {
		if ag, err := database.GetAgent(ctx, db, target.Agent); err == nil && strings.TrimSpace(ag.Endpoint) != "" {
			baseURL = ag.Endpoint
			log.Printf("DISPATCH: %s tick target agent %s uses its REGISTERED relay endpoint %s (targeted dispatch)",
				proj.Name, target.Agent, baseURL)
		}
	}
	if baseURL == "" {
		return client.Dispatch(ctx, target.Agent, item)
	}
	return client.DispatchTo(ctx, baseURL, target.Agent, item)
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

	// The session deadline a local tick would have had, computed ONCE
	// (effectiveTickTimeout records feature use on wave-enabled namespaces, so
	// a second call would double-count). It bounds the await below AND travels
	// to the node as limits.wall_s (CF-138), so the far end cannot kill work
	// the near end is still waiting for.
	timeout := p.spawner.effectiveTickTimeout(proj)

	item := bus.WorkItem{
		Lane:    proj.Name,
		Board:   board,
		Workdir: workdir,
		CorrID:  client.NextCorrID(),
		// The scheduler's OWN inbox: the proven fleet dispatcher answers
		// payload.reply_to, so naming ourselves is what makes the reply
		// arrive where the correlation id can be matched.
		ReplyTo: client.InboxAgentID(),
		// The DIRECTIVE (CF-139). Identical to what a local tick receives, so
		// the remote harness is asked to do the lane's work instead of being
		// handed the envelope and told to run it.
		Text: buildForemanPrompt(proj, tickID),
		// The same deadline the await below uses. The node must stop before
		// we do, not at some fixed constant of its own (CF-138).
		WallS: int(timeout.Seconds()),
	}

	receipt, err := dispatchToTarget(dctx, client, proj, target, item, db)
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
	// wall is printed because it decides how long the node is allowed to work
	// and how long we wait (CF-138's one clock). Without it an operator cannot
	// tell a 15-minute budget from a 3-hour one after the fact.
	log.Printf("DISPATCH: %s tick=%s agent=%s corr=%s message=%s transport=%s wall=%s lane_ref=%s board_ref=%s",
		proj.Name, tickID, receipt.AgentID, receipt.CorrID, receipt.MessageID, receipt.Transport, timeout, workdir, board)

	// Wait for the answer, bounded by the same session deadline a local tick
	// would have had (computed above, and already carried to the node as
	// limits.wall_s). The db rides along so an uncorrelatable message can be
	// checked against every OUTSTANDING hand-out before it is consumed
	// (SCHED-GAP-16984).
	reply, ok, inbox, waitErr := p.awaitDispatchReply(dctx, proj, tickID, receipt, db, timeout)

	finished := p.clock().Now()
	rec.UpdatedAt = finished.UTC().Format(time.RFC3339Nano)
	rec.Reply = reply
	// SCHED-GAP-16984: the inbox correlation accounting rides in the tick
	// detail whenever there is something to report (an uncorrelatable message
	// would otherwise be invisible); a matched reply with no stray traffic
	// keeps the receipt byte-identical to the pre-16984 shape.
	if ann := inbox.annotate(); ann != "" {
		if rec.Reply == "" {
			rec.Reply = ann
		} else {
			rec.Reply += "\n" + ann
		}
	}
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
// context ends.
//
// SCHED-GAP-16984 — the poll is no longer silent about the messages it cannot
// correlate. Every message is counted (seen / correlated / uncorrelated),
// each uncorrelatable message is named by the correlation key it claims
// (bounded, so a hostile inbox cannot blow up a log line), and is then
// EXPLICITLY SETTLED: acked when it is true residue, deferred with a spelled
// reason when it may still belong to someone — another tick's outstanding
// hand-out, no lease, ack error, past deadline. ONE structured line is logged
// per retrieve batch that carried messages (never per message):
//
//	DISPATCH: <proj> tick=<id> inbox seen=<n> correlated=<n> uncorrelated=<n> keys=<csv> acked=<n> deferred=<n>
//
// and the batch totals accumulate into the returned dispatchInboxStats, which
// the caller records on the tick's dispatch receipt. The same events bump the
// process-wide counters surfaced on /api/v1/metrics as dispatch_inbox. The
// matched-reply behaviour — the verdict, the ack, the return values — is
// byte-identical to the pre-16984 leg; a matched reply still wins over
// everything else in the batch, and the rest of the batch is settled instead
// of being left leased to TTL.
func (p *SlotPool) awaitDispatchReply(ctx context.Context, proj PackedProject, tickID string, receipt bus.DispatchReceipt, db *sql.DB, budget time.Duration) (string, bool, dispatchInboxStats, error) {
	client := p.busClientForDispatch()
	if client == nil || !client.InboxEnabled() {
		return "", false, dispatchInboxStats{}, fmt.Errorf("%w: no inbox identity while awaiting the answer to %s", ErrDispatchNotConfigured, receipt.CorrID)
	}
	if budget <= 0 {
		budget = DefaultGatewayResponseTimeout
	}
	deadline := p.clock().Now().Add(budget)
	deadlineErr := func() error {
		return fmt.Errorf("no correlated reply for corr=%s (message=%s) within %s — the hand-out is recorded and the agent may still answer",
			receipt.CorrID, receipt.MessageID, budget.Round(time.Millisecond))
	}
	var stats dispatchInboxStats
	for {
		remaining := deadline.Sub(p.clock().Now())
		if remaining <= 0 {
			return "", false, stats, deadlineErr()
		}
		// InboxRetrieve has its own longer request timeout. Bound it by the
		// remaining tick budget, or a long-poll can overrun a short tick
		// deadline (and hold the slot until the bus client's default timeout).
		retrieveCtx, retrieveCancel := context.WithTimeout(ctx, remaining)
		lease, err := client.InboxRetrieve(retrieveCtx, dispatchReplyBatchLimit, dispatchReplyLeaseSeconds)
		retrieveDeadline := errors.Is(retrieveCtx.Err(), context.DeadlineExceeded)
		retrieveCancel()
		if err != nil {
			if ctx.Err() != nil {
				return "", false, stats, fmt.Errorf("dispatch %s: %w", receipt.CorrID, ctx.Err())
			}
			if retrieveDeadline || errors.Is(err, context.DeadlineExceeded) {
				return "", false, stats, deadlineErr()
			}
			// A bus blip is not a verdict about the work: log and keep
			// waiting until the budget runs out.
			log.Printf("DISPATCH: %s tick=%s inbox retrieve: %v (retrying)", proj.Name, tickID, err)
		} else {
			batch := dispatchInboxStats{}
			var replyText string
			replyOK := false
			replied := false
			for _, msg := range lease.Messages {
				stats.Seen++
				batch.Seen++
				payload, matched := correlateDispatchReply(msg, receipt)
				if matched {
					stats.Correlated++
					batch.Correlated++
					remaining := deadline.Sub(p.clock().Now())
					if remaining > 0 {
						ackCtx, ackCancel := context.WithTimeout(ctx, remaining)
						if aerr := client.InboxAck(ackCtx, lease.LeaseID, msg.ID); aerr != nil {
							log.Printf("DISPATCH: %s tick=%s ack %s: %v", proj.Name, tickID, msg.ID, aerr)
						} else {
							// Consumed by its owner (this or a sibling
							// await): drop any residue belief held for the
							// id (SCHED-GAP-16984 unacked accounting).
							dispatchInboxUnackedRelease(msg.ID)
						}
						ackCancel()
					}
					// First correlated message wins, exactly as before; a
					// duplicate late answer for the same hand-out is
					// consumed above instead of rotting on the lease.
					if !replied {
						replied = true
						ok, why := dispatchReplyVerdict(payload)
						text := dispatchReplyText(payload)
						if !ok {
							if strings.TrimSpace(text) == "" {
								text = why
							}
							replyText, replyOK = text, false
						} else {
							replyText, replyOK = text, true
						}
					}
					continue
				}
				// SCHED-GAP-16984: a message that names no hand-out of ours
				// is reported, then settled — never silently leased to TTL.
				key := dispatchReplyKey(msg)
				stats.Uncorrelated++
				batch.Uncorrelated++
				stats.Keys = appendBoundedDispatchKey(stats.Keys, &stats.KeysOverflow, key)
				batch.Keys = appendBoundedDispatchKey(batch.Keys, &batch.KeysOverflow, key)
				outcome := p.settleDispatchInboxResidue(ctx, client, db, lease, msg, deadline)
				stats.settle(outcome)
				batch.settle(outcome)
			}
			if batch.Seen > 0 {
				// ONE line per batch that carried messages, and the same
				// events into the per-process counters.
				recordDispatchInboxBatch(batch)
				line := fmt.Sprintf("DISPATCH: %s tick=%s inbox seen=%d correlated=%d uncorrelated=%d keys=%s acked=%d deferred=%d",
					proj.Name, tickID, batch.Seen, batch.Correlated, batch.Uncorrelated,
					boundedDispatchCSV(batch.Keys, batch.KeysOverflow), batch.Acked, batch.Deferred)
				if len(batch.DeferredReasons) > 0 {
					line += " deferred_reasons=" + boundedDispatchCSV(batch.DeferredReasons, batch.DeferredOverflow)
				}
				log.Print(line)
			}
			if replied {
				return replyText, replyOK, stats, nil
			}
		}
		remaining = deadline.Sub(p.clock().Now())
		if remaining <= 0 {
			return "", false, stats, deadlineErr()
		}
		poll := dispatchReplyPollInterval
		if poll > remaining {
			poll = remaining
		}
		select {
		case <-ctx.Done():
			return "", false, stats, fmt.Errorf("dispatch %s: %w", receipt.CorrID, ctx.Err())
		case <-p.clock().After(poll):
		}
	}
}

// dispatchInboxStats accumulates the reply-inbox correlation accounting for
// one awaitDispatchReply (stats) or one retrieve batch (batch): every message
// seen, how many correlated to the hand-out, how many did not, the
// correlation keys the uncorrelated ones claimed (bounded by
// maxDispatchInboxKeys plus an overflow count), and how the uncorrelated ones
// were settled (acked / deferred with a reason, bounded the same way).
type dispatchInboxStats struct {
	Seen             int
	Correlated       int
	Uncorrelated     int
	Keys             []string
	KeysOverflow     int
	Acked            int
	Deferred         int
	DeferredReasons  []string
	DeferredOverflow int
}

// settle records one settleDispatchInboxResidue outcome ("acked" or
// "deferred: <reason>") into the accumulator.
func (s *dispatchInboxStats) settle(outcome string) {
	if strings.HasPrefix(outcome, dispatchSettleAcked) {
		s.Acked++
		return
	}
	s.Deferred++
	s.DeferredReasons = appendBoundedDispatchKey(s.DeferredReasons, &s.DeferredOverflow, outcome)
}

// annotate renders the inbox-correlation summary the tick's dispatch receipt
// carries. Empty by design when nothing uncorrelated was seen: a matched
// reply with no stray traffic keeps the receipt byte-identical to the
// pre-16984 shape, while any uncorrelatable message is spelled out — counts,
// claimed keys, and how each left the lease — rather than left silent.
func (s dispatchInboxStats) annotate() string {
	if s.Uncorrelated == 0 {
		return ""
	}
	line := fmt.Sprintf("[dispatch-inbox] seen=%d correlated=%d uncorrelated=%d keys=%s acked=%d deferred=%d",
		s.Seen, s.Correlated, s.Uncorrelated, boundedDispatchCSV(s.Keys, s.KeysOverflow), s.Acked, s.Deferred)
	if len(s.DeferredReasons) > 0 {
		line += " deferred_reasons=" + boundedDispatchCSV(s.DeferredReasons, s.DeferredOverflow)
	}
	return line
}

// dispatchSettleAcked prefixes a settled-ack outcome.
const dispatchSettleAcked = "acked"

// maxDispatchInboxKeys bounds how many correlation keys / defer reasons one
// accumulator retains (per batch, per tick detail). Beyond the bound the
// events still COUNT — only their names stop being retained — so a hostile
// inbox cannot blow up a log line while the totals stay exact.
const maxDispatchInboxKeys = 10

func appendBoundedDispatchKey(list []string, overflow *int, key string) []string {
	if len(list) >= maxDispatchInboxKeys {
		*overflow++
		return list
	}
	return append(list, key)
}

// boundedDispatchCSV renders a bounded key list; overflow is spelled out as a
// trailing "+N-more" element so a reader knows names were dropped.
func boundedDispatchCSV(list []string, overflow int) string {
	if overflow <= 0 {
		return strings.Join(list, ",")
	}
	if len(list) == 0 {
		return fmt.Sprintf("+%d-more", overflow)
	}
	return strings.Join(list, ",") + fmt.Sprintf(",+%d-more", overflow)
}

// dispatchReplyKey names the correlation identity an inbox message CLAIMS —
// the key it would have had to carry to correlate, extracted even when the
// payload is unreadable — so the log line and the tick detail name what was
// seen instead of a bare count (SCHED-GAP-16984).
func dispatchReplyKey(msg bus.InboxMessage) string {
	suffix := "no-id"
	if strings.TrimSpace(msg.ID) != "" {
		suffix = msg.ID
	}
	payload, err := msg.Payload()
	if err != nil || payload == nil {
		return "unreadable:" + suffix
	}
	if s, _ := payload["corr_id"].(string); s != "" {
		return s
	}
	if s, _ := payload["in_reply_to"].(string); s != "" {
		return s
	}
	if echoed, ok := payload["task"].(map[string]any); ok {
		if id, _ := echoed["corr_id"].(string); id != "" {
			return id
		}
	}
	return "unidentified:" + suffix
}

// truncateDispatchReason keeps a defer reason readable without letting a
// hostile error string blow up the log line or the tick detail.
func truncateDispatchReason(s string) string {
	const max = 160
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// settleDispatchInboxResidue decides the fate of one inbox message that did
// not correlate to THIS tick's hand-out (SCHED-GAP-16984): true residue is
// acked so the inbox cannot accumulate unclassifiable leases; anything that
// may still belong to someone is deferred with the reason spelled out. The
// return value is exactly "acked" or "deferred: <reason>". The same events
// maintain the process-wide unacked-residue belief set.
func (p *SlotPool) settleDispatchInboxResidue(ctx context.Context, client *bus.Client, db *sql.DB, lease bus.InboxLease, msg bus.InboxMessage, deadline time.Time) string {
	if lease.LeaseID == "" {
		dispatchInboxUnackedAdd(msg.ID)
		return "deferred: no lease id"
	}
	if msg.ID == "" {
		return "deferred: no message id"
	}
	payload, perr := msg.Payload()
	corrID, inReplyTo := "", ""
	if perr == nil && payload != nil {
		corrID, _ = payload["corr_id"].(string)
		inReplyTo, _ = payload["in_reply_to"].(string)
	}
	owned, oerr := dispatchReplyOwnedElsewhere(ctx, db, corrID, inReplyTo)
	switch {
	case oerr != nil:
		dispatchInboxUnackedAdd(msg.ID)
		return "deferred: ownership check failed: " + truncateDispatchReason(oerr.Error())
	case owned:
		// A live answer to ANOTHER tick's outstanding hand-out: that tick's
		// own await may still correlate it. Not residue — left for its owner
		// (the short lease returns it to the inbox) and never acked here.
		return "deferred: answers an in-flight hand-out"
	}
	remaining := deadline.Sub(p.clock().Now())
	if remaining <= 0 {
		dispatchInboxUnackedAdd(msg.ID)
		return "deferred: past deadline"
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, remaining)
	aerr := client.InboxAck(ackCtx, lease.LeaseID, msg.ID)
	ackCancel()
	if aerr != nil {
		dispatchInboxUnackedAdd(msg.ID)
		return "deferred: ack error: " + truncateDispatchReason(aerr.Error())
	}
	dispatchInboxUnackedRelease(msg.ID)
	return dispatchSettleAcked
}

// dispatchReplyOwnedElsewhere reports whether the identity a message claims
// names an OUTSTANDING hand-out of this scheduler — a tick_dispatch row still
// in state=dispatched, i.e. work whose answer another await may still be
// polling for. Such a message is not residue and must not be consumed. The
// current tick's own receipt cannot false-positive here: the message already
// failed to correlate against it. A nil db cannot answer the question, which
// is itself the error (fail-closed: never consume what may be a sibling's
// answer).
func dispatchReplyOwnedElsewhere(ctx context.Context, db *sql.DB, corrID, inReplyTo string) (bool, error) {
	if db == nil {
		return false, errors.New("no db to check outstanding hand-outs")
	}
	if corrID == "" && inReplyTo == "" {
		return false, nil
	}
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM tick_dispatch WHERE state = ? AND (corr_id = ? OR message_id = ?) LIMIT 1`,
		database.DispatchStateDispatched, corrID, inReplyTo).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("query outstanding hand-outs: %w", err)
	}
	return true, nil
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
		if s := decodedReplyField(payload, "error_b64"); s != "" {
			return false, s
		}
		for _, k := range []string{"error", "err", "reply", "text", "message"} {
			if s, ok := payload[k].(string); ok && strings.TrimSpace(s) != "" {
				return false, s
			}
		}
		return false, "the agent reported ok=false"
	}
	return true, ""
}

// decodedReplyField returns the base64-encoded member `key` as text, or "".
func decodedReplyField(payload map[string]any, key string) string {
	enc, _ := payload[key].(string)
	if enc == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return ""
	}
	return string(raw)
}

// dispatchReplyText extracts the agent's answer text. An answer that is not a
// string is rendered as JSON rather than dropped: an opaque reply is still a
// reply, and the operator can read it.
func dispatchReplyText(payload map[string]any) string {
	// The completion travels base64 (reply_b64) when the agent's own answer
	// quotes JSON — a foreman report names board rows and curl bodies, and the
	// relay's pre-match refuses that shape 403 GUARD_BLOCKED on the way home.
	// Same reason the directive is encoded outbound.
	if enc, ok := payload["reply_b64"].(string); ok && enc != "" {
		if raw, err := base64.StdEncoding.DecodeString(enc); err == nil {
			return string(raw)
		}
	}
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

// DispatchInboxSnapshot is the per-process snapshot of the dispatch reply
// inbox's correlation accounting (SCHED-GAP-16984), returned by
// DispatchInboxCounters. Like Loop.AdmissionCounters
// (the SCHED-GAP-155 precedent) the counters are in-memory process state,
// monotonic since daemon boot and reset by a restart — there is no persisted
// equivalent, and inventing one from the ticks table would fabricate numbers
// this endpoint is contractually forbidden from inventing.
type DispatchInboxSnapshot struct {
	// SeenTotal: every inbox message a dispatch reply await examined.
	SeenTotal int64
	// CorrelatedTotal: messages matched to the awaiting tick's hand-out.
	CorrelatedTotal int64
	// UncorrelatedTotal: messages that matched no hand-out (the residue the
	// row exists to make visible; each is named in the scheduler log and in
	// the awaiting tick's dispatch receipt).
	UncorrelatedTotal int64
	// AckedTotal: uncorrelated messages consumed with an ack.
	AckedTotal int64
	// DeferredTotal: uncorrelated messages explicitly deferred with a reason
	// (in-flight hand-out, no lease, ack error, past deadline).
	DeferredTotal int64
	// UnackedCurrent: the process's belief of how many uncorrelated messages
	// are STILL parked without an ack — deferred ones, minus any later seen
	// successfully acked or consumed by their owner. A steady nonzero value
	// is the inbox-residue alarm.
	UnackedCurrent int64
	// UnackedOverflow: identities dropped once the belief set hit
	// dispatchInboxUnackedMax (the count stays honest; the identity tracking
	// is bounded).
	UnackedOverflow int64
}

// dispatchInboxUnackedMax bounds the unacked-residue belief set.
const dispatchInboxUnackedMax = 4096

// dispatchInboxMetricsState is the process-wide counter set behind
// DispatchInboxCounters. One daemon owns one SlotPool, so per-process (not
// per-pool) state is the honest granularity; a mutex (not atomics) keeps the
// belief-set updates and the snapshot trivially race-free at one bump per
// retrieve batch.
type dispatchInboxMetricsState struct {
	mu              sync.Mutex
	seen            int64
	correlated      int64
	uncorrelated    int64
	acked           int64
	deferred        int64
	unackedOverflow int64
	unacked         map[string]struct{}
}

var processDispatchInbox = dispatchInboxMetricsState{unacked: map[string]struct{}{}}

// recordDispatchInboxBatch folds one retrieve batch's settled accounting into
// the process counters.
func recordDispatchInboxBatch(b dispatchInboxStats) {
	processDispatchInbox.mu.Lock()
	defer processDispatchInbox.mu.Unlock()
	processDispatchInbox.seen += int64(b.Seen)
	processDispatchInbox.correlated += int64(b.Correlated)
	processDispatchInbox.uncorrelated += int64(b.Uncorrelated)
	processDispatchInbox.acked += int64(b.Acked)
	processDispatchInbox.deferred += int64(b.Deferred)
}

// dispatchInboxUnackedAdd records that an uncorrelated message was deferred
// (kept) — it is believed still parked unacked until released. Re-adding an
// already-tracked id (the same message failing to ack across batches) is not
// a new residue.
func dispatchInboxUnackedAdd(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	processDispatchInbox.mu.Lock()
	defer processDispatchInbox.mu.Unlock()
	if _, ok := processDispatchInbox.unacked[id]; ok {
		return
	}
	if len(processDispatchInbox.unacked) >= dispatchInboxUnackedMax {
		processDispatchInbox.unackedOverflow++
		return
	}
	processDispatchInbox.unacked[id] = struct{}{}
}

// dispatchInboxUnackedRelease drops the residue belief for an id that was
// successfully acked (by this settle, or later by its owning await).
func dispatchInboxUnackedRelease(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	processDispatchInbox.mu.Lock()
	defer processDispatchInbox.mu.Unlock()
	delete(processDispatchInbox.unacked, id)
}

// DispatchInboxCounters returns the process-wide snapshot. Always populated:
// before any dispatch the counters read as honest zeros, never an invented
// number.
func DispatchInboxCounters() DispatchInboxSnapshot {
	processDispatchInbox.mu.Lock()
	defer processDispatchInbox.mu.Unlock()
	return DispatchInboxSnapshot{
		SeenTotal:         processDispatchInbox.seen,
		CorrelatedTotal:   processDispatchInbox.correlated,
		UncorrelatedTotal: processDispatchInbox.uncorrelated,
		AckedTotal:        processDispatchInbox.acked,
		DeferredTotal:     processDispatchInbox.deferred,
		UnackedCurrent:    int64(len(processDispatchInbox.unacked)),
		UnackedOverflow:   processDispatchInbox.unackedOverflow,
	}
}
