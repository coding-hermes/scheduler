package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"log"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1602 — authentication for the scheduler control surface.
//
// THREAT MODEL (decided, in writing): the surface is bound to 127.0.0.1 and
// tailnet-forwarded (sched-ui-tailnet socat). The fleet has ONE human
// operator. The defended threat is UNAUTHENTICATED CONTROL — any process on
// any machine that can reach the port (22 mutating routes: fleet-wide pause,
// project/namespace/group/template delete) must be refused. It is explicitly
// NOT a hostile multi-tenant user model: no accounts, no sessions, no
// per-identity authorization. One shared operator credential asserts "a
// device the operator armed", which is exactly the guarantee the exposure
// posture calls for.
//
// CREDENTIAL POSTURE (decided, in writing): the dashboard is plain HTTP over
// the tailnet — WireGuard-encrypted in transit but an INSECURE ORIGIN to the
// browser. Therefore no cookie and no token stored in browser JS storage is
// used (either would be CSRF-able or stealable, strictly worse than no auth);
// the credential travels ONLY in a request header. The browser gets it via
// HTTP basic (one credential, browser-cached for the session); API callers
// may send Basic, a Bearer token, or the raw X-Operator-Token header. The
// HTTPS-origin gap is SCHED-GAP-1600's row, not this one — nothing here
// changes when that lands.
//
// ROUTE SPLIT (decided, in writing): every MUTATING route requires the
// operator credential. Reads (status, health, live, metrics, queue, ticks,
// events, config, openapi) stay OPEN: the consumers that poll them (cron
// probes, the ops watchdog, Observatory-style pullers, the dashboard's own
// render path) are read-only observers, and the data they see — fleet shape,
// tick history — is already published off-box by the DuckBrain sync. Gating
// reads would break every one of those consumers for no security gain: the
// mutation is the danger, not the observation.
//
// FAIL-CLOSED: no configured credential means mutations are REFUSED (503),
// never allowed. A blank/whitespace credential is treated as unset.
//
// AUDIT: every mutating call — allowed or refused — writes an events-table
// record (component "api.auth") naming the caller identity (or "anonymous"),
// the method+path, the resolved target and the outcome. Allowed entries are
// INFO, refusals are MEDIUM. Read back via GET /api/v1/events?component=api.auth.

// authMode enumerates the configured credential modes.
type authMode int

const (
	// authOff means NO credential is configured. Fail-closed: mutations 503.
	authOff authMode = iota
	// authToken means a shared operator token is configured (Bearer /
	// X-Operator-Token header, or Basic with the token as the password).
	authToken
	// authBasic means an operator name + password pair is configured
	// (HTTP basic; the browser prompt path).
	authBasic
)

// Auth outcome vocabulary (SCHED-GAP-1619): the shared classification one
// decision ladder writes into audit rows on BOTH control surfaces — REST
// (requireOperator) and MCP (the tools/call mutation gate). Consumers filter
// events by these exact strings via GET /api/v1/events?component=….
const (
	AuthOutcomeAllowed                = "allowed"
	AuthOutcomeRefusedUnauthenticated = "refused-unauthenticated"
	AuthOutcomeRefusedBadCredential   = "refused-bad-credential"
	AuthOutcomeRefusedNoCredential    = "refused-no-credential"
)

// String renders the mode for events and introspection. It never leaks
// credential material — only the mode name.
func (m authMode) String() string {
	switch m {
	case authToken:
		return "token"
	case authBasic:
		return "basic"
	default:
		return "off"
	}
}

// authConfig is the resolved operator-authentication configuration. It is
// immutable after installation (built once by resolveAuthConfig); handlers
// read it without locking.
type authConfig struct {
	mode authMode
	// token is the shared operator token (authToken mode). Empty otherwise.
	token string
	// user is the basic-auth username (authBasic mode). Empty otherwise.
	user string
	// password is the basic-auth password (authBasic mode). Empty otherwise.
	password string
}

// resolveAuthConfig derives the effective auth configuration from the three
// config layers main.go already resolved. An empty --operator-token flag
// means "not set", so env and TOML keep the same precedence the other knobs
// use. Token mode wins when both modes are configured — one credential to
// rotate beats two. Blank/whitespace values are treated as UNSET
// (fail-closed), never as a credential.
func resolveAuthConfig(flagToken, envToken, tomlToken, basicUser, basicPassword string) authConfig {
	tok := firstNonBlank(flagToken, envToken, tomlToken)
	if tok != "" {
		return authConfig{mode: authToken, token: tok}
	}
	if firstNonBlank(basicUser) != "" && firstNonBlank(basicPassword) != "" {
		return authConfig{mode: authBasic, user: basicUser, password: basicPassword}
	}
	return authConfig{mode: authOff}
}

// firstNonBlank returns the first value that is non-empty after trimming
// spaces; a whitespace-only value counts as unset.
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// authenticateRequest checks the request against the configured credential
// and returns the caller identity to audit. ok=false means refuse.
//
// Accepted shapes:
//   - Authorization: Bearer <token>            (token mode)
//   - Authorization: Basic base64(user:token)  (token mode: the user part is
//     ignored, the token must be the password — `curl -u "":<token>` and
//     `curl -u operator:<token>` both work)
//   - Authorization: Basic base64(user:password) (basic mode; user must match)
//   - X-Operator-Token: <token>                (token mode, and also accepted
//     in basic mode so one script shape works against either configuration)
//
// Comparison is constant-time (crypto/subtle). The identity recorded in the
// audit event is "operator:token"/"operator:basic" for an accepted
// credential — the model asserts the operator's device, not a person.
func (a authConfig) authenticateRequest(r *http.Request) (identity string, ok bool) {
	switch a.mode {
	case authToken:
		if tok := extractBearerOrHeaderToken(r); tok != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) == 1 {
				return "operator:token", true
			}
			return "invalid", false
		}
		return "anonymous", false
	case authBasic:
		if user, pass, had := r.BasicAuth(); had {
			userOK := subtle.ConstantTimeCompare([]byte(user), []byte(a.user)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(a.password)) == 1
			if userOK && passOK {
				return "operator:basic", true
			}
			return "invalid", false
		}
		if tok := r.Header.Get("X-Operator-Token"); tok != "" {
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(a.password)) == 1 {
				return "operator:token", true
			}
			return "invalid", false
		}
		return "anonymous", false
	default:
		// authOff — the caller (requireOperator) refuses before consulting
		// the credential; this arm exists so authenticateRequest alone never
		// silently admits anyone.
		return "anonymous", false
	}
}

// extractBearerOrHeaderToken pulls the bearer token or X-Operator-Token
// header value. Basic credentials are handled by the caller (BasicAuth)
// because they carry a user part.
func extractBearerOrHeaderToken(r *http.Request) string {
	if h := r.Header.Get("X-Operator-Token"); h != "" {
		return strings.TrimSpace(h)
	}
	auth := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(auth, "Bearer "):
		return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	case strings.HasPrefix(auth, "Basic "):
		if _, pass, ok := r.BasicAuth(); ok {
			return pass
		}
	}
	return ""
}

// writeAuthRefused answers a refused mutating request. Status semantics:
//   - 401 Unauthorized: a credential was presented and was WRONG, or none was
//     presented while one is required. WWW-Authenticate is set in basic mode
//     so the browser prompts.
//   - 503 Service Unavailable: NO credential is configured (fail-closed
//     misconfiguration arm). 503 rather than 401 because there is nothing
//     the caller can send — the daemon refuses to operate unauthenticated,
//     it is not challenging for a password.
func writeAuthRefused(w http.ResponseWriter, reason string, challenge bool, code int) {
	if challenge {
		w.Header().Set("WWW-Authenticate", `Basic realm="scheduler operator"`)
	}
	w.Header().Set("X-Operator-Auth", "required")
	writeError(w, code, reason)
}

// authChallenge reports whether refusals should advertise WWW-Authenticate
// (drives the browser's native prompt for basic mode). Token mode stays
// quiet: a browser prompt for a header token is a dead end.
func (a authConfig) authChallenge() bool {
	return a.mode == authBasic
}

// SCHED-GAP-1619 — the minimal exported surface the MCP mutation gate reads.
// MCP must reuse ONE resolved credential configuration and ONE decision
// ladder; these three accessors expose exactly what the JSON-RPC layer needs
// (no credential material ever leaves the package).

// Evaluate authenticates a request against the configured credential using
// the SAME constant-time checks and accepted header shapes as the REST
// mutation gate. Returns the caller identity ("operator:token" /
// "operator:basic" / "anonymous" / "invalid") and whether the call is
// admitted. It NEVER returns credentials; only a classification.
func (a authConfig) Evaluate(r *http.Request) (identity string, ok bool) {
	return a.authenticateRequest(r)
}

// Mode reports the configured mode name ("token" / "basic" / "off") — the
// same string the REST audit rows carry in their mode field. It never leaks
// credential material.
func (a authConfig) Mode() string { return a.mode.String() }

// IsOff reports the fail-closed misconfiguration arm: no operator credential
// is configured. Both gates (REST and MCP) refuse every mutation while this
// is true, regardless of what the caller presents.
func (a authConfig) IsOff() bool { return a.mode == authOff }

// mutationAudit describes one mutating call for the audit record.
type mutationAudit struct {
	Identity string // operator:token / operator:basic / anonymous / invalid
	Method   string
	Path     string
	Target   string // the object the mutation names (project, namespace id, …)
	Outcome  string // allowed / refused-unauthenticated / refused-bad-credential / refused-no-credential
	Mode     string // authMode.String() at decision time
}

// buildAuditEvent renders the mutationAudit into an events row. Message
// carries the human sentence; Details carries JSON so dashboard readers can
// filter. Deterministic (no clock read — LogEvent stamps created_at).
func buildAuditEvent(a mutationAudit) *database.Event {
	severity := database.SeverityInfo
	if a.Outcome != "allowed" {
		severity = database.SeverityMedium
	}
	return &database.Event{
		Severity:  severity,
		Component: "api.auth",
		Message:   "auth: " + a.Outcome + " " + a.Method + " " + a.Path + " target=" + a.Target + " identity=" + a.Identity,
		Details:   "{\"identity\":\"" + a.Identity + "\",\"method\":\"" + a.Method + "\",\"path\":\"" + a.Path + "\",\"target\":\"" + a.Target + "\",\"outcome\":\"" + a.Outcome + "\",\"mode\":\"" + a.Mode + "\"}",
	}
}

// auditMutation persists one audit record via database.LogEvent — the SAME
// mechanism /api/v1/events and /api/v1/events/stream serve. Best-effort by
// contract: an audit-write failure must not turn an executed mutation into a
// 500 (the mutation already happened), but it must be VISIBLE, hence the
// loud log line with the payload inline (the row is reconstructible).
func auditMutation(ctx context.Context, db *sql.DB, a mutationAudit) {
	e := buildAuditEvent(a)
	if err := database.LogEvent(ctx, db, e); err != nil {
		log.Printf("AUTH AUDIT WRITE FAILED: severity=%s component=%s msg=%q details=%q err=%v",
			e.Severity, e.Component, e.Message, e.Details, err)
	}
}

// ResolveAuthConfig builds the auth configuration from the credentials
// main.go resolved (token: SCHEDULER_OPERATOR_TOKEN env > [api]
// operator_token TOML; basic: [api] operator_user/operator_password TOML
// only; NO flag layer by design). Token mode wins when both are configured.
// An empty credential set leaves authOff — the fail-closed mode where the
// mutation gate refuses every mutating request with 503.
func ResolveAuthConfig(token, basicUser, basicPassword string) authConfig {
	return resolveAuthConfig(token, "", "", basicUser, basicPassword)
}
