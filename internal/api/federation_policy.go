package api

// REMOTE-013 (docs/federation-query-spec.md §4, "Access model"): the READ
// access policy for the federation query surface.
//
// THE LAW (spec §4, "Read is scoped, not open"): a peer answers only ops its
// operator has published; absence of an allow is a REFUSAL, never a default-
// yes. The zero policy (SetFederationReadPolicy never called — every unit
// test server, a daemon booted without [federation] allows) answers NOTHING:
// every federation read is `status="error" code="op_not_allowed"` until the
// operator explicitly publishes ops. This is the SCHED-GAP-1602 posture
// carried to the read path: the gate fails CLOSED on the missing case, and
// a blank/whitespace entry is treated as unset (never as an allow).
//
// SHAPE (spec §4, "Per-peer scoping"): the allow-list is keyed by the
// CALLER's identity — the primary may ask peer A more than peer B. The
// special caller "*" names the fleet-wide default every caller inherits;
// a caller's own row wins when both exist. "*" has no built-in meaning to
// the enforcement below beyond being a key like any other, so an operator
// who never configures it gets exactly the callers they named.
//
// ENFORCEMENT (one place, by construction): the ONE check —
// federationReadPolicy.allows — is called from the TWO entry surfaces that
// exist (handleFederationQuery for HTTP; federationExecQuery, the
// transport-agnostic core FederationQueryHandler runs for bus + MCP + CLI).
// There is no per-transport copy to drift: an op added to the catalogue is
// covered by construction, and REMOTE-014's battery would catch a transport
// that answers outside the shared core.
//
// AUDIT (spec §4, "Cross-box read audit"): every REFUSED read writes the
// same api.federation events row an answered read writes (the refusal is
// the interesting event) — through the ONE audit path each surface already
// uses (federationAudit / federationExecAudit), never a second mechanism.
//
// CONFIG (spec §4's published-vocabulary): the daemon resolves the policy
// once at boot from the root config ([federation] allow / allow.<caller>)
// via ResolveFederationReadPolicy and arms it with
// SetFederationReadPolicy — the same three-layer shape the auth
// configuration uses (internal/api/auth.go). There is deliberately no CLI
// flag: an access policy is per-deployment, not per-invocation (GAP-038
// family: the policy is authority, not a credential, but the same
// "configure the daemon, not the command" law keeps the surface honest).

import (
	"strings"
)

// FederationAllowAll is the sentinel caller key naming the fleet-wide
// default grant. A caller row ("primary-01") wins over the "*" row, so an
// operator can publish a broad default and then scope a specific peer
// tighter — or broader — per caller.
const FederationAllowAll = "*"

// FederationReadPolicy is the resolved per-caller read policy: which caller
// may read which ops of THIS scheduler. The zero value is the deny-all
// policy (fail-closed) — it is safe by construction, never by
// configuration. Immutable after SetFederationReadPolicy; the query
// surfaces read it without locking.
type FederationReadPolicy struct {
	// allow maps caller identity → the set of published op ids. An absent
	// caller key means that caller may read NOTHING (spec §4: absence of
	// an allow is a refusal). An op present in the set is answerable; any
	// other op is op_not_allowed — including ops that exist in the
	// catalogue: the catalogue is what COULD be answered, the policy is
	// what MAY be.
	allow map[string]map[string]bool
}

// FederationReadGrant is one caller's published allow-list, the resolved
// form of one `[federation.allow]` table row.
type FederationReadGrant struct {
	// Caller is the caller identity the grant applies to: "*" (the
	// FederationAllowAll default) or a specific caller id as it appears in
	// the audit rows ("bus:transport", "mcp:auth-token", "primary-01", …).
	Caller string
	// Ops is the published op set. Blank/whitespace entries are dropped at
	// resolve time (a blank allow is unset, never a grant).
	Ops []string
}

// ResolveFederationReadPolicy builds the immutable policy from the resolved
// grants. Trims every op id; drops blank entries; nil/empty input yields the
// deny-all policy. The SAME op id spelling as the catalogue is the
// operator's contract — an entry that never matches a catalogue op is
// stored as given (it simply can never allow anything) so a typo is visible
// in the config snapshot instead of silently normalised away.
func ResolveFederationReadPolicy(grants []FederationReadGrant) FederationReadPolicy {
	p := FederationReadPolicy{allow: map[string]map[string]bool{}}
	for _, g := range grants {
		caller := strings.TrimSpace(g.Caller)
		if caller == "" {
			// A grant with no caller would be an unattributable authority —
			// drop it rather than guessing an identity for it.
			continue
		}
		set, ok := p.allow[caller]
		if !ok {
			set = map[string]bool{}
			p.allow[caller] = set
		}
		for _, op := range g.Ops {
			if t := strings.TrimSpace(op); t != "" {
				set[t] = true
			}
		}
	}
	return p
}

// allows is THE decision (fail-closed): may caller read op? Absence of an
// allow is a refusal — the empty, the unknown, and the unpublished op all
// answer false. The caller's own row wins over the "*" default when both
// exist (per-peer scoping: a specific grant overrides the fleet-wide one in
// BOTH directions — a caller with an own row is not silently widened by
// "*", and a caller without one inherits it).
func (p FederationReadPolicy) allows(caller, op string) bool {
	if p.allow == nil {
		return false
	}
	if set, ok := p.allow[caller]; ok {
		return set[op]
	}
	if set, ok := p.allow[FederationAllowAll]; ok {
		return set[op]
	}
	return false
}

// SetFederationReadPolicy arms the read policy. main.go calls it once at
// boot with the [federation] layer resolved by ResolveFederationReadPolicy.
// A Server that never receives it stays deny-all: every federation read on
// every transport answers op_not_allowed (fail-closed, spec §4) — exactly
// the posture SetAuthConfig establishes for mutations.
func (s *Server) SetFederationReadPolicy(p FederationReadPolicy) {
	s.fedPolicy = p
}

// federationReadPolicy returns the armed policy; the zero value (never
// armed) is the deny-all policy by construction.
func (s *Server) federationReadPolicy() FederationReadPolicy {
	return s.fedPolicy
}

// federationCheckRead is the ONE shared read-policy gate. Both entry
// surfaces call it — handleFederationQuery (HTTP) and federationExecQuery
// (bus + MCP + CLI, via FederationQueryHandler) — so every transport is
// covered by construction (spec §4 per-peer scoping; the REMOTE-013 law:
// one check, not four parallel ones that can drift).
//
// An op OUTSIDE the catalogue (and outside fleet.aggregate) passes through:
// it is not a federation read at all, and the unknown_op refusal (spec
// §2.2) owns that answer — the policy governs real reads, it does not
// reclassify typos. Everything else answers the stable refusal envelope
// (status="error" code="op_not_allowed") unless the caller's policy allows
// the op. The gate does NOT audit: each entry surface audits the refusal
// through its own existing §4 audit call, keeping one audit path per
// surface and none invented.
func (s *Server) federationCheckRead(caller, op string) *federationError {
	if _, known := federationOpByName(op); !known && op != fedOpAggregate {
		return nil
	}
	if s.federationReadPolicy().allows(caller, op) {
		return nil
	}
	return &federationError{
		Code: fedErrOpNotAllowed,
		Message: "op " + op + " is not published for caller " + caller +
			" (spec §4: reads are scoped, not open — see [federation] allow config)",
	}
}
