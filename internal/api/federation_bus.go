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

// fedErrUnknownCaller is the envelope-level refusal for a query that
// arrives without a caller identity (the MCP transport derives its caller
// from the resolved auth mode — "off" has no caller name). Same refusal
// family as missing_op/missing_corr_id: a named answer, never a guess.
const fedErrUnknownCaller = "unknown_caller"

// fedErrOpNotAllowed is the REMOTE-013 read-policy refusal code (spec §4:
// "anything else is status=\"error\" code=\"op_not_allowed\" — a named
// refusal, never an empty answer"): the caller is authenticated but the op
// is not published for it in the per-caller read policy
// (FederationReadPolicy / the [federation] allow config). Stable vocabulary:
// remoting callers branch on this string.
const fedErrOpNotAllowed = "op_not_allowed"

// federationExecQuery is the ONE transport-agnostic query entry point (spec
// §3: "Every adapter: parses its surface → builds the envelope → calls the
// SAME internal query entry point → returns the SAME response"). It holds
// the adapter-independent core every transport shares — want validation →
// unknown-op refusal (catalogue inline) → budget-clamped read deadline →
// the op's read → ok/partial downgrade → replay store. The envelope is the
// contract; the transport-specific parts (rendering, auditing with a named
// caller, the HTTP writer) stay in the adapters.
//
// Returns the transport-independent §2.2 envelope (status possibly "stale"
// via federationResponse's freshness law) or, on the refusal/deadline arms,
// a status="error" envelope built by federationEnvelopeError — the two
// adapters then transcribe/render it into their own wire shapes.
func (s *Server) federationExecQuery(q *queryEnvelope, caller string) (resp responseEnvelope) {
	// Spec §4: every answered query — allowed OR refused — writes one
	// local audit row ("reads are observable, not silently free"). The
	// named-return audit runs on EVERY arm below. The outcome stamped is
	// the error CODE when the envelope carries one (a REMOTE-013 policy
	// refusal is audited as op_not_allowed, a timeout as
	// deadline_exceeded — the refusal is the interesting event, and the
	// row names the outcome the way the HTTP surface's rows do); plain
	// ok/partial/stale answers stamp the status word.
	defer func() {
		outcome := resp.Status
		if resp.Error != nil && resp.Error.Code != "" {
			outcome = resp.Error.Code
		}
		s.federationExecAudit(caller, q.CorrID, q.Op, outcome)
	}()

	// REMOTE-013 (spec §4, "Read is scoped, not open"): the read-policy
	// gate — the ONE shared check handleFederationQuery runs on the HTTP
	// surface, so bus, MCP and CLI (the callers of
	// FederationQueryHandler → federationExecQuery) are covered by the
	// same decision the HTTP wire answers with. Absence of an allow is a
	// REFUSAL (op_not_allowed), never a default-yes; the refusal renders
	// as a normal §2.2 error envelope (named, never silence) and the
	// deferred audit above records it. Checked BEFORE want/unknown-op so
	// an unpublished op is indistinguishable from a nonexistent one to an
	// unauthorized caller (the refusal does not disclose the catalogue).
	if fedErrPolicy := s.federationCheckRead(caller, q.Op); fedErrPolicy != nil {
		return s.federationEnvelopeError(q, fedErrOpNotAllowed, fedErrPolicy.Message)
	}

	// REMOTE-013 (spec §4, "Read is scoped, not open"): the read-policy
	// gate — the ONE shared check handleFederationQuery runs on the HTTP
	// surface, so bus, MCP and CLI (the callers of
	// FederationQueryHandler → federationExecQuery) are covered by the
	// same decision the HTTP wire answers with. Absence of an allow is a
	// REFUSAL (op_not_allowed), never a default-yes; the refusal renders
	// as a normal §2.2 error envelope (named, never silence) and the
	// deferred audit above records it. The caller identity comes FIRST
	// (an unknown caller is refused before authorization — the auth
	// ladder's order); the catalogue-unknown op passes through to the
	// unknown_op refusal below (the policy governs real reads, it does
	// not reclassify typos).
	if strings.TrimSpace(caller) == "" {
		return s.federationEnvelopeError(q, fedErrUnknownCaller, "caller identity is required")
	}
	if fedErrPolicy := s.federationCheckRead(caller, q.Op); fedErrPolicy != nil {
		return s.federationEnvelopeError(q, fedErrOpNotAllowed, fedErrPolicy.Message)
	}

	// want vocabulary (spec §2.1) — same named refusal as HTTP.
	want := strings.TrimSpace(q.Want)
	if want == "" {
		want = fedWantAnswer
	}
	if want != fedWantAnswer && want != fedWantPartial {
		return s.federationEnvelopeError(q, fedErrBadRequest,
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
		return s.federationEnvelopeError(q, fedErrUnknownOp,
			"unknown op "+q.Op+" — supported: "+strings.Join(names, ", "))
	}

	// The read deadline: the caller's budget_ms (peer default when 0 or
	// absent), clamped exactly like the HTTP surface clamps it (spec §2.1
	// "the peer clamps to its own cap").
	ctx, obs := s.federationRequestDeadline(context.Background(), q.budgetMS())
	defer obs.finish()

	// Replay window FIRST (spec §2.5): a redelivery of the same
	// (caller, corr_id, op) returns the FIRST answer — the identical
	// stored bytes, as_of included — instead of re-reading.
	replay := s.federationReplayWindow()
	if hit, ok := replay.lookup(caller, q.CorrID, q.Op); ok {
		var prior responseEnvelope
		if err := json.Unmarshal(hit.body, &prior); err == nil {
			return prior
		}
		// A stored body that no longer decodes must never surface as a
		// wrong answer — fall through and answer fresh (a second store
		// under the same key is a no-op, so the FIRST answer stays).
		log.Printf("FEDERATION: replay body undecodable (corr %s) — answering fresh", q.CorrID)
	}

	data, gaps, fedErr := fn(ctx, s, q)
	if !obs.checkBus(ctx) {
		return s.federationEnvelopeError(q, fedErrDeadlineExceed,
			"read budget exhausted ("+ctx.Err().Error()+")")
	}
	if fedErr != nil {
		return s.federationEnvelopeError(q, fedErr.Code, fedErr.Message)
	}
	status := fedStatusOK
	if len(gaps) > 0 {
		// A gap in the answer downgrades ok → partial (spec §2.2/§5 law).
		status = fedStatusPartial
	}
	resp = s.federationResponse(q, status, data, gaps)
	// Store BEFORE returning (the HTTP handler's discipline): an answer
	// lost in transit is still replayable.
	replay.store(caller, q.CorrID, q.Op, http.StatusOK, resp)
	return resp
}

// federationEnvelopeError renders the status="error" variant of the §2.2
// envelope (the fedWriteError twin, minus the HTTP writer) — the refusal
// shape the bus and MCP adapters share verbatim, and the shape
// federationExecQuery answers its refusal arms with.
func (s *Server) federationEnvelopeError(q *queryEnvelope, code, message string) responseEnvelope {
	return responseEnvelope{
		CorrID:   q.CorrID,
		Op:       q.Op,
		Peer:     database.SchedulerID(),
		Status:   fedStatusError,
		Gaps:     make([]federationGap, 0),
		Error:    &federationError{Code: code, Message: message},
		Contract: federationContractVersion,
	}
}

// FederationBusHandler answers one bus query envelope by running the SAME
// internal query entry point POST /api/v1/federation/query runs: the want
// validation → replay window → the op's read under a budget_ms-derived
// deadline → the §2.2 envelope. NO HTTP objects are touched; the answer is
// the plain envelope the bus responder publishes back on the correlated
// reply topic.
func (s *Server) FederationBusHandler(q bus.QueryEnvelope) bus.ResponseEnvelope {
	return s.FederationQueryHandler(q, FederationBusCaller)
}

// FederationQueryHandler is THE internal query entry point for the
// non-HTTP transport adapters (spec §3: every adapter "builds the envelope
// → calls the SAME internal query entry point → returns the SAME
// response"). The bus adapter and the MCP adapter (REMOTE-010) both route
// here; the envelope members are the spec §2.1 contract carried verbatim,
// and the answer is the §2.2 envelope in the bus wire shape — the exact
// member set every transport serves (the §6 conformance duty).
//
// caller is the spec §4 audit/replay identity of the requesting transport
// (the bus uses FederationBusCaller; MCP names its resolved auth mode) —
// it keys the shared replay window and the audit row, and an empty caller
// is a named unknown_caller refusal (fail-closed, never a guessed
// identity). It also keys the REMOTE-013 read policy (spec §4 per-peer
// scoping): a caller may read only the ops published for it in the
// [federation] allow config; anything else is the named op_not_allowed
// refusal, audited like every answer.
func (s *Server) FederationQueryHandler(q bus.QueryEnvelope, caller string) bus.ResponseEnvelope {
	env := queryEnvelope{Op: q.Op, Args: q.Args, CorrID: q.CorrID, BudgetMS: q.BudgetMS, Want: q.Want}
	resp := s.federationExecQuery(&env, caller)
	return s.federationBusResponse(&env, resp)
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

// federationBusResponse transcribes the transport-independent §2.2 envelope
// into the bus shape, member-for-member (the gap TYPE differs across
// packages; the JSON member set is the same §2.2 set). The freshness/stale
// law was already applied upstream in federationResponse.
func (s *Server) federationBusResponse(q *queryEnvelope, resp responseEnvelope) bus.ResponseEnvelope {
	return bus.ResponseEnvelope{
		CorrID:   resp.CorrID,
		Op:       resp.Op,
		Peer:     resp.Peer,
		Status:   resp.Status,
		AsOf:     resp.AsOf,
		AgeMS:    resp.AgeMS,
		Data:     resp.Data,
		Gaps:     busGapsFrom(resp.Gaps),
		Error:    busErrorFrom(resp.Error),
		Contract: resp.Contract,
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

// busErrorFrom transcribes the api error object into the bus shape (nil
// stays nil — a successful envelope carries no error member).
func busErrorFrom(e *federationError) *bus.FederationError {
	if e == nil {
		return nil
	}
	return &bus.FederationError{Code: e.Code, Message: e.Message}
}

// federationExecAudit writes the spec §4 cross-box read audit row for an
// answer produced by the shared transport-agnostic entry point
// (federationExecQuery) — the same events mechanism the HTTP surface
// audits through, with the requesting transport NAMED in the caller
// identity ("bus:transport", "mcp:auth-<mode>", …). Runs for allowed AND
// refused answers (the refusal is the interesting event). Best-effort: an
// audit failure never fails an answer, and the read deadline is NOT used
// so an answered query is audited even when its budget was tight. The
// deadline context is not threaded here (the entry point's ctx may already
// be cancelled by the time a 504 renders).
func (s *Server) federationExecAudit(caller, corrID, op, outcome string) {
	msg := "federation.query: " + outcome + " op=" + op + " corr_id=" + corrID + " caller=" + caller
	e := &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api.federation",
		Message:   msg,
		Details:   "{\"caller\":\"" + caller + "\",\"corr_id\":\"" + corrID + "\",\"op\":\"" + op + "\",\"outcome\":\"" + outcome + "\",\"path\":\"fed.shared\"}",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.LogEvent(ctx, s.db, e); err != nil {
		log.Printf("FEDERATION AUDIT WRITE FAILED: msg=%q err=%v", msg, err)
	}
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
