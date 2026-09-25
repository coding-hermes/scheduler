package api

import (
	"context"
	"net/http"
)

// SCHED-GAP-1602 — mutation gate.
//
// One choke point between the mux and the mutating handlers. It decides —
// per request — whether the call is a MUTATION (identity required), runs the
// credential check, writes the audit record, and stashes the decision for
// the handler. Fail-closed is structural: with authOff, every mutating
// request is refused with 503 BEFORE any handler code runs.

// operatorIdentity is the request-state key under which requireOperator
// stashes the authenticated caller identity for the handler to record.
type operatorIdentity struct{}

// IdentityFromContext returns the authenticated caller identity
// ("operator:token" / "operator:basic"), or "" when the request never passed
// the gate (reads, or a Server without SetAuthConfig).
func IdentityFromContext(ctx context.Context) string {
	v, _ := ctx.Value(operatorIdentity{}).(string)
	return v
}

// requireOperator is THE mutation gate. Called at the top of every mutating
// handler; false means the refusal response has already been written (and
// audited) and the handler must return without touching state.
//
// Refusal statuses (writeAuthRefused):
//   - authOff            → 503 fail-closed (misconfiguration arm)
//   - missing credential → 401 (unauthenticated arm)
//   - wrong credential   → 401 (bad-credential arm)
//
// Every decision — allowed or refused — writes an api.auth events row.
func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request, target string) bool {
	cfg := s.authConfig()
	identity, ok := cfg.authenticateRequest(r)

	audit := mutationAudit{
		Identity: identity,
		Method:   r.Method,
		Path:     r.URL.Path,
		Target:   target,
		Mode:     cfg.mode.String(),
	}

	switch {
	case cfg.mode == authOff:
		audit.Outcome = "refused-no-credential"
		auditMutation(r.Context(), s.db, audit)
		writeAuthRefused(w,
			"mutations disabled: no operator credential configured "+
				"(set SCHEDULER_OPERATOR_TOKEN / [api] operator_token)",
			false, http.StatusServiceUnavailable)
		return false
	case ok:
		audit.Outcome = "allowed"
		auditMutation(r.Context(), s.db, audit)
		// Hand the identity to the handler via the request context so the
		// success path can record WHO without re-checking anything.
		*r = *r.WithContext(context.WithValue(r.Context(), operatorIdentity{}, identity))
		return true
	case identity == "invalid":
		audit.Outcome = "refused-bad-credential"
		auditMutation(r.Context(), s.db, audit)
		writeAuthRefused(w, "invalid operator credential", cfg.authChallenge(), http.StatusUnauthorized)
		return false
	default:
		audit.Outcome = "refused-unauthenticated"
		auditMutation(r.Context(), s.db, audit)
		writeAuthRefused(w, "operator credential required for mutations", cfg.authChallenge(), http.StatusUnauthorized)
		return false
	}
}
