package api

// REMOTE-012 (docs/federation-query-spec.md §5): the AGGREGATE — the
// primary's fan-out of one §2.1 envelope over every known peer, merged into
// one §2.2 envelope by the spec's merge rules:
//
//   - ok blocks merge (each peer's data is preserved per peer — no invented
//     cross-peer aggregation, spec §7 non-goal);
//   - any partial/error/timeout becomes a NAMED gap for that peer and the
//     overall status drops to partial (the caller is never told a federated
//     answer is complete when a peer went silent);
//   - per-peer degradation is explicit: every known peer appears as a row
//     with its own status. A peer that timed out is status="error"
//     code="timeout", NEVER an omitted row; the registry's rendering law
//     (docs/remote-spec.md §2) rides beside it — stale=true plus the raw
//     last_contact instant, never a "down" state.
//
// THE ADDRESSING (one contract, no new surface): the aggregate is the same
// POST /api/v1/federation/query envelope with op="fleet.aggregate" and
// args.op naming the §2.3 read op to fan out — the remaining args members
// pass through to every leg verbatim. It therefore inherits the surface's
// operator gate (spec §4), the replay window (§2.5) and the read-audit row
// (§4) without a second route to keep honest. Peers do NOT answer
// fleet.aggregate: it is not in the §2.3 catalogue, and a peer asked about
// the fleet answers unknown_op (spec §7: only the primary aggregates; a
// peer answers about itself).
//
// THE CEILING (deliverable 1): bounded, concurrent fan-out —
// fedAggregateConcurrency legs in flight, each leg under its own per-peer
// deadline derived from the caller's budget_ms, and a collector timer at
// the budget so one slow or hanging peer can never hold the answer hostage:
// an unfilled leg when the ceiling fires renders as a named timeout row.
// If EVERY peer is unreachable the primary STILL ANSWERS — every row
// error/timeout, every peer named in the gaps, the envelope non-empty. An
// empty aggregate is a bug.
//
// The primary's OWN answer is included (deliverable 2): the self row is
// computed by the same read function the peer surfaces run, through
// federationResponse — so the self-freshness law (status="stale") applies
// to the aggregator's own row exactly as it does to a single-peer answer.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// fedOpAggregate is the envelope op that addresses the aggregate (§5). It
// is deliberately NOT in federationCatalogue / federationOps: the catalogue
// is what a PEER answers about itself (§2.3), and the aggregate is what the
// primary computes over its registry. The contract version stays 1.0.0 —
// no §2.2 member changed shape; this is an additive envelope op.
const fedOpAggregate = "fleet.aggregate"

// fedAggregateConcurrency bounds the fan-out (deliverable 1): larger
// registries queue on the semaphore; every leg still gets its full per-peer
// budget once scheduled.
const fedAggregateConcurrency = 8

// fedAggregateSlackMS is the extra wall time the aggregate's request
// deadline grants over the per-peer budget for scheduling the legs and
// merging. The collector stops at the per-peer budget (each leg enforces
// its own deadline); the slack is the never-hang margin that lets the
// envelope assemble inside the request deadline instead of tripping the
// surface's 504 arm.
const fedAggregateSlackMS = 2000

// AggregateTransport is the ask-side seam (REMOTE-012): ask ONE peer one
// §2.1 envelope and get the §2.2 envelope or a named error. Production
// wires bus.Client.Query (REMOTE-009's ask side — cmd/schedulerd/main.go);
// tests inject stubs. An error wrapping bus.ErrQueryTimeout classifies as
// the §5 timeout degradation; any other error is a named transport error.
type AggregateTransport func(ctx context.Context, peerID string, q bus.QueryEnvelope) (bus.ResponseEnvelope, error)

// aggregatePeerRow is one peer's row of the aggregate (§5: "the aggregate
// lists every peer with its own status"). Data/error/as_of ride only when
// the peer produced them; stale + last_contact are the registry's rendering
// law and are present on every row (a silent peer is stale with a
// last-contact time — "" when it never heartbeat — never "down").
type aggregatePeerRow struct {
	Peer        string           `json:"peer"`
	Self        bool             `json:"self"`
	Status      string           `json:"status"`
	Stale       bool             `json:"stale"`
	LastContact string           `json:"last_contact"`
	AsOf        string           `json:"as_of,omitempty"`
	AgeMS       *int64           `json:"age_ms,omitempty"`
	Data        any              `json:"data,omitempty"`
	Gaps        []federationGap  `json:"gaps"`
	Error       *federationError `json:"error,omitempty"`
}

// SetAggregateTransport installs the ask-side transport the aggregate fans
// out with. main.go wires the bus client once at boot; nil (tests, bare
// servers) is a documented posture — the aggregate still answers, with
// every peer rendered as a named transport_unavailable row.
func (s *Server) SetAggregateTransport(t AggregateTransport) {
	s.aggTransport = t
}

// federationAggregateArgs extracts the leg arguments: args["op"] names the
// read op to fan out; the remaining members pass through to every leg
// verbatim (the §5 "SAME envelope" law).
func federationAggregateArgs(args map[string]any) (subOp string, legArgs map[string]any) {
	subOp = argStringOf(args, "op")
	legArgs = make(map[string]any, len(args))
	for k, v := range args {
		if k == "op" {
			continue
		}
		legArgs[k] = v
	}
	return subOp, legArgs
}

// argStringOf reads args[key] as a trimmed string ("" when absent/wrong
// type) — the same discipline queryEnvelope.argString applies, for the
// aggregate's own arg extraction.
func argStringOf(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// federationAggregateRun executes the aggregate (§5) and returns the §2.2
// envelope. Every arm — refusal or answer — is non-hanging and never empty:
// the envelope always exists, and the data always names every peer.
//
// parent is the caller's context (the HTTP request context on the route);
// every leg derives from it, so a caller hang-up tears the fan-out down.
func (s *Server) federationAggregateRun(q *queryEnvelope, parent context.Context) responseEnvelope {
	subOp, legArgs := federationAggregateArgs(q.Args)
	if subOp == "" {
		return s.federationEnvelopeError(q, fedErrBadRequest,
			`args.op is required — the §2.3 read op to aggregate (e.g. {"op":"fleet.aggregate","args":{"op":"queue.get"}})`)
	}
	if _, ok := federationOpByName(subOp); !ok {
		names := make([]string, 0, len(federationCatalogue))
		for _, e := range federationCatalogue {
			names = append(names, e.Op)
		}
		return s.federationEnvelopeError(q, fedErrUnknownOp,
			"unknown aggregate op "+subOp+" — supported: "+strings.Join(names, ", "))
	}

	peers, err := database.ListPeers(parent, s.db)
	if err != nil {
		return s.federationEnvelopeError(q, fedErrInternal, "ListPeers: "+err.Error())
	}

	// The ceiling: the caller's budget_ms arms the whole fan-out's request
	// deadline (+ slack for scheduling the legs and the merge), each leg's
	// own deadline is the budget, and the collector timer fires at the
	// budget — inside the request deadline, so the envelope assembles in
	// the slack window instead of tripping the 504 arm. A peer clamps to
	// its own cap (spec §2.1): the budget rides the leg envelope.
	ceiling := q.budgetMS()
	ctx, obs := s.federationRequestDeadline(parent, ceiling+fedAggregateSlackMS)
	defer obs.finish()

	gaps := make([]federationGap, 0)
	rows := make([]*aggregatePeerRow, 0, len(peers)+1)
	answers := make(map[string]any, len(peers)+1)

	// ── the primary's own answer first (deliverable 2) ─────────────────
	selfRow := s.federationAggregateSelfRow(ctx, q, subOp, legArgs, ceiling)
	rows = append(rows, selfRow)
	if selfRow.Data != nil {
		answers[selfRow.Peer] = selfRow.Data
	}
	if selfRow.Status != fedStatusOK {
		gaps = append(gaps, federationGap{What: "peer " + selfRow.Peer + " (self)", Why: rowGapWhy(selfRow)})
	}

	// ── the bounded concurrent fan-out (deliverable 1) ─────────────────
	// Each leg owns one slot and reports through a buffered channel (one
	// slot per leg — a late leg can always deposit and exit; nothing ever
	// blocks). The collector stops at the ceiling timer: whatever has not
	// landed by then renders as a named timeout row — a hanging peer is
	// structurally unable to hold the answer hostage.
	type legOut struct {
		i   int
		row *aggregatePeerRow
	}
	out := make(chan legOut, len(peers))
	sem := make(chan struct{}, fedAggregateConcurrency)
	for i := range peers {
		go func(i int) {
			sem <- struct{}{}
			defer func() { <-sem }()
			out <- legOut{i, s.federationAggregateLeg(ctx, peers[i], subOp, legArgs, ceiling, q.Want)}
		}(i)
	}
	filled := make([]*aggregatePeerRow, len(peers))
	timer := s.clock().NewTimer(time.Duration(ceiling) * time.Millisecond)
	defer timer.Stop()
	for remaining := len(peers); remaining > 0; remaining-- {
		select {
		case o := <-out:
			filled[o.i] = o.row
		case <-timer.C:
			remaining = 0 // stop collecting; unfilled slots render below
		}
	}

	// Registry order for the output (stable, like GET /api/v1/peers).
	for i, p := range peers {
		row := filled[i]
		if row == nil {
			// The ceiling fired before this leg answered: a NAMED timeout
			// row — never an omitted peer, never a hang.
			row = &aggregatePeerRow{
				Peer:        p.ID,
				Status:      fedStatusError,
				Stale:       s.peerIsStale(p),
				LastContact: p.LastContact,
				Gaps:        make([]federationGap, 0),
				Error: &federationError{Code: "timeout",
					Message: "aggregate ceiling reached before the peer answered"},
			}
		}
		rows = append(rows, row)
		if row.Data != nil {
			answers[row.Peer] = row.Data
		}
		switch row.Status {
		case fedStatusOK:
			// complete
		case fedStatusStale:
			// A stale peer is a FULL first-class answer (spec §2.2) — it
			// degrades neither the row's completeness nor the aggregate,
			// but its staleness is named so the caller can judge it.
			gaps = append(gaps, federationGap{What: "peer " + row.Peer,
				Why: "stale: " + federationStaleWhy(p.LastContact, s.peerFreshnessWindow(), s.clock().Now)})
		default:
			gaps = append(gaps, federationGap{What: "peer " + row.Peer, Why: rowGapWhy(row)})
		}
	}

	// ── the merge (§5): ok blocks merge; any partial/error/timeout drops
	// the overall status to partial with the gap named ───────────────────
	status := fedStatusOK
	if len(gaps) > 0 {
		status = fedStatusPartial
	}
	answered, degraded := 0, 0
	for _, row := range rows {
		switch row.Status {
		case fedStatusOK, fedStatusStale:
			answered++
		default:
			degraded++
		}
	}
	data := map[string]any{
		"op":         subOp,
		"peer_count": len(rows),
		"answered":   answered,
		"degraded":   degraded,
		"peers":      rows,
		"answers":    answers,
	}
	return s.federationResponse(q, status, data, gaps)
}

// rowGapWhy renders the named gap for a non-ok row: the stable error code
// plus its message, or the bare status when no error object rode along.
func rowGapWhy(row *aggregatePeerRow) string {
	if row.Error != nil {
		return row.Error.Code + ": " + row.Error.Message
	}
	return "status " + row.Status
}

// federationAggregateSelfRow computes the primary's own row with the SAME
// read function the peer surfaces run (spec §3: no answer computed outside
// the shared entry point's dispatch table), under its own per-read
// deadline, rendered through federationResponse so the freshness/stale law
// applies to the aggregator's own row.
func (s *Server) federationAggregateSelfRow(ctx context.Context, q *queryEnvelope, subOp string, legArgs map[string]any, legBudget int) *aggregatePeerRow {
	fn, _ := federationOpByName(subOp)
	legQ := queryEnvelope{Op: subOp, Args: legArgs, CorrID: q.CorrID, BudgetMS: &legBudget, Want: q.Want}
	legCtx, cancel := context.WithTimeout(ctx, time.Duration(legBudget)*time.Millisecond)
	defer cancel()
	row := &aggregatePeerRow{
		Peer:   database.SchedulerID(),
		Self:   true,
		Status: fedStatusOK,
		Gaps:   make([]federationGap, 0),
	}
	data, gaps, fedErr := fn(legCtx, s, &legQ)
	if fedErr != nil {
		row.Status = fedStatusError
		row.Error = fedErr
		return row
	}
	status := fedStatusOK
	if len(gaps) > 0 {
		status = fedStatusPartial
	}
	resp := s.federationResponse(&legQ, status, data, gaps)
	row.Status = resp.Status
	row.AsOf = resp.AsOf
	age := resp.AgeMS
	row.AgeMS = &age
	row.Data = resp.Data
	row.Gaps = resp.Gaps
	row.Stale = resp.Status == fedStatusStale
	return row
}

// federationAggregateLeg asks ONE peer and renders its row. A transport
// failure degrades to the named error row (§5 vocabulary: timeout for a
// silent peer; the registry's stale/last_contact ride beside it) — never a
// dropped peer, never a hang past the leg's own deadline.
func (s *Server) federationAggregateLeg(ctx context.Context, p database.Peer, subOp string, legArgs map[string]any, legBudget int, want string) *aggregatePeerRow {
	row := &aggregatePeerRow{
		Peer:        p.ID,
		Stale:       s.peerIsStale(p),
		LastContact: p.LastContact,
		Gaps:        make([]federationGap, 0),
	}
	transport := s.aggTransport
	if transport == nil {
		row.Status = fedStatusError
		row.Error = &federationError{Code: "transport_unavailable",
			Message: "no federation ask-side transport is wired on this scheduler"}
		return row
	}
	legCtx, cancel := context.WithTimeout(ctx, time.Duration(legBudget)*time.Millisecond)
	defer cancel()
	budget := legBudget
	busQ := bus.QueryEnvelope{Op: subOp, Args: legArgs, BudgetMS: &budget, Want: want}
	resp, err := transport(legCtx, p.ID, busQ)
	if err != nil {
		row.Status = fedStatusError
		row.Error = &federationError{Code: transportErrCode(err), Message: err.Error()}
		return row
	}
	row.Status = resp.Status
	row.AsOf = resp.AsOf
	age := resp.AgeMS
	row.AgeMS = &age
	row.Data = resp.Data
	// Transcribe the bus wire shapes into the api shapes (the §2.2 member
	// sets are identical; the Go types differ across packages — same
	// discipline federationBusResponse applies in the other direction).
	row.Gaps = make([]federationGap, 0, len(resp.Gaps))
	for _, g := range resp.Gaps {
		row.Gaps = append(row.Gaps, federationGap{What: g.What, Why: g.Why})
	}
	if resp.Error != nil {
		row.Error = &federationError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	return row
}

// peerIsStale applies THE freshness predicate (database.IsPeerStale) with
// this server's resolved window — the same definition GET /api/v1/peers
// renders with.
func (s *Server) peerIsStale(p database.Peer) bool {
	return database.IsPeerStale(p.LastContact, s.peerFreshnessWindow(), s.clock().Now)
}

// transportErrCode maps a transport error to the §5 stable code vocabulary
// (the same words the CLI's degradation envelope uses).
func transportErrCode(err error) string {
	switch {
	case errors.Is(err, bus.ErrQueryTimeout):
		return "timeout"
	case errors.Is(err, bus.ErrReplyClosed):
		return "reply_closed"
	case errors.Is(err, bus.ErrDisabled):
		return "bus_disabled"
	default:
		return "transport_error"
	}
}

// federationStaleWhy renders WHY a peer is stale — the honest reason (never
// heartbeat vs. heartbeat older than the window), not a bare boolean.
func federationStaleWhy(lastContact string, windowSeconds int, now func() time.Time) string {
	if strings.TrimSpace(lastContact) == "" {
		return "peer never heartbeat (registered, no liveness stamp)"
	}
	if t, err := time.Parse(time.RFC3339, lastContact); err == nil {
		return fmt.Sprintf("last heartbeat %s ago exceeds the %ds freshness window", now().Sub(t).Round(time.Second), windowSeconds)
	}
	return "last_contact unparseable: " + lastContact
}
