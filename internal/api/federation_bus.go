package api

// REMOTE-009 (docs/federation-query-spec.md §3, "Crier bus" row): the bus
// adapter for the federation QUERY surface. The envelope is the contract
// and the BUS carries it verbatim in both directions (internal/bus.Query /
// ResponseEnvelope — the same §2.1/§2.2 JSON member set as the HTTP wire);
// what this file adds is the HAND-OFF into the ONE internal query entry
// point (federationOps + the shared replay window) so a bus answer and the
// HTTP answer for the same op are computed by the same code (spec §3 law:
// "An adapter that computes an answer itself is a bug").
//
// Note on the RESPONSE envelope (spec §6 conformance): the §6 battery
// normalizes the transport-specific members — `peer` (both surfaces stamp
// the same SchedulerID, but a bus reply may originate from a different
// machine than the test's HTTP call) and the freshness pair (`as_of`/
// `age_ms` are stamped at answer time, so two answers differ by the wall
// clock between them). `data`, `status`, `gaps` and `contract` are the
// contract and are compared EXACTLY — this file shares the freshness/stale
// decision (federationResponse) and the read functions with the HTTP
// handler so they can only drift by a code change, never silently.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/bus"
	"github.com/coding-hermes/scheduler/internal/database"
)

// FederationBusHandler answers one bus query envelope by running the SAME
// internal query entry point POST /api/v1/federation/query runs: the want
// validation → replay window → the op's read under a budget_ms-derived
// deadline → the §2.2 envelope. NO HTTP objects are touched; the answer is
// the plain envelope the bus responder publishes back on the correlated
// reply topic.
func (s *Server) FederationBusHandler(q bus.QueryEnvelope) bus.ResponseEnvelope {
	// want vocabulary (spec §2.1) — same named refusal as HTTP.
	want := strings.TrimSpace(q.Want)
	if want == "" {
		want = fedWantAnswer
	}
	if want != fedWantAnswer && want != fedWantPartial {
		return s.federationBusError(q, fedErrBadRequest,
			"want must be \""+fedWantAnswer+"\" or \""+fedWantPartial+"\"")
	}
	// Unknown op: a NAMED refusal with the catalogue inline (spec §2.2),
	// mirroring the HTTP handler's answer.
	fn, ok := federationOpByName(q.Op)
	if !ok {
		names := make([]string, 0, len(federationCatalogue))
		for _, e := range federationCatalogue {
			names = append(names, e.Op)
		}
		return s.federationBusError(q, fedErrUnknownOp,
			"unknown op "+q.Op+" — supported: "+strings.Join(names, ", "))
	}

	// The read deadline: the caller's budget_ms (peer default when 0 or
	// absent), clamped exactly like the HTTP surface clamps it (spec §2.1
	// "the peer clamps to its own cap").
	env := queryEnvelope{Op: q.Op, Args: q.Args, CorrID: q.CorrID, BudgetMS: q.BudgetMS, Want: want}
	ctx, obs := s.federationRequestDeadline(context.Background(), env.budgetMS())
	defer obs.finish()

	// Replay window FIRST (spec §2.5): a bus redelivery of the same
	// (caller, corr_id, op) returns the FIRST answer — the identical
	// stored bytes, as_of included — instead of re-reading.
	caller := FederationBusCaller
	replay := s.federationReplayWindow()
	if hit, ok := replay.lookup(caller, q.CorrID, q.Op); ok {
		var prior bus.ResponseEnvelope
		if err := json.Unmarshal(hit.body, &prior); err == nil {
			return prior
		}
		// A stored body that no longer decodes must never surface as a
		// wrong answer — fall through and answer fresh (a second store
		// under the same key is a no-op, so the FIRST answer stays).
		log.Printf("FEDERATION BUS: replay body undecodable (corr %s) — answering fresh", q.CorrID)
	}

	data, gaps, fedErr := fn(ctx, s, &env)
	if !obs.checkBus(ctx) {
		return s.federationBusError(q, fedErrDeadlineExceed,
			"read budget exhausted ("+ctx.Err().Error()+")")
	}
	if fedErr != nil {
		return s.federationBusError(q, fedErr.Code, fedErr.Message)
	}
	status := fedStatusOK
	if len(gaps) > 0 {
		// A gap in the answer downgrades ok → partial (spec §2.2/§5 law).
		status = fedStatusPartial
	}
	resp := s.federationBusResponse(&env, status, data, gaps)
	// Store BEFORE returning (the HTTP handler's discipline): an answer
	// lost in transit is still replayable. The store takes the HTTP-shaped
	// envelope (the window is shared with the HTTP surface), and the bus
	// shapes marshal member-for-member identically, so a replayed bus
	// answer decodes back into bus.ResponseEnvelope unchanged.
	replay.store(caller, q.CorrID, q.Op, http.StatusOK, s.federationResponse(&env, status, data, gaps))
	s.federationBusAudit(caller, q.CorrID, q.Op, status)
	return resp
}

// federationBusError renders the status="error" variant of the §2.2
// envelope for the bus (the fedWriteError twin, minus the HTTP writer).
func (s *Server) federationBusError(q bus.QueryEnvelope, code, message string) bus.ResponseEnvelope {
	return bus.ResponseEnvelope{
		CorrID: q.CorrID,
		Op:     q.Op,
		Peer:   database.SchedulerID(),
		Status: fedStatusError,
		Gaps:   []bus.FederationGap{},
		Error:  &bus.FederationError{Code: code, Message: message},
		// The §2.6 contract version — fed from the HTTP surface's
		// constant so the two transports can never disagree.
		Contract: federationContractVersion,
	}
}

// federationBusResponse renders the §2.2 envelope for the bus, sharing the
// freshness/stale law with the HTTP path (federationResponse) and
// transcribing member-for-member into the bus shape (the gap TYPE differs
// across packages; the JSON member set is the same §2.2 set).
func (s *Server) federationBusResponse(q *queryEnvelope, status string, data any, gaps []federationGap) bus.ResponseEnvelope {
	httpShaped := s.federationResponse(q, status, data, gaps)
	return bus.ResponseEnvelope{
		CorrID:   httpShaped.CorrID,
		Op:       httpShaped.Op,
		Peer:     httpShaped.Peer,
		Status:   httpShaped.Status,
		AsOf:     httpShaped.AsOf,
		AgeMS:    httpShaped.AgeMS,
		Data:     httpShaped.Data,
		Gaps:     busGapsFrom(httpShaped.Gaps),
		Error:    nil,
		Contract: httpShaped.Contract,
	}
}

// busGapsFrom transcribes the api gap type into the bus shape.
func busGapsFrom(gaps []federationGap) []bus.FederationGap {
	out := make([]bus.FederationGap, 0, len(gaps))
	for _, g := range gaps {
		out = append(out, bus.FederationGap{What: g.What, Why: g.Why})
	}
	return out
}

// checkBus is the bus-shaped deadline arm of the request observer: the same
// ctx.Err decision as checkFederation, minus the HTTP writer. Returns false
// when the read budget is exhausted (the caller must answer the timeout
// envelope — status="error" code="deadline_exceeded", the §5 degradation).
func (d *requestDeadline) checkBus(ctx context.Context) bool {
	if d == nil {
		return true
	}
	d.closeStep()
	return ctx.Err() == nil
}

// FederationBusCaller is the caller identity bus queries are audited and
// replay-keyed under: the bus transport itself (the relay's opt-in bearer
// is the transport credential — spec §4 "The bus carries the same opt-in
// bearer"; the answering side does not re-authenticate the requesting
// scheduler per query).
const FederationBusCaller = "bus:transport"

// federationBusAudit writes the spec §4 cross-box read audit row for a bus
// answer — the same events mechanism the HTTP surface audits through, with
// the transport named. Best-effort (an audit failure never fails an answer;
// the deadline context is NOT used so an answered query is audited even
// when its budget was tight).
func (s *Server) federationBusAudit(caller, corrID, op, outcome string) {
	msg := "federation.query: " + outcome + " op=" + op + " corr_id=" + corrID + " caller=" + caller
	e := &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api.federation",
		Message:   msg,
		Details:   "{\"caller\":\"" + caller + "\",\"corr_id\":\"" + corrID + "\",\"op\":\"" + op + "\",\"outcome\":\"" + outcome + "\",\"path\":\"bus:fed.query\"}",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.LogEvent(ctx, s.db, e); err != nil {
		log.Printf("FEDERATION BUS AUDIT WRITE FAILED: msg=%q err=%v", msg, err)
	}
}
