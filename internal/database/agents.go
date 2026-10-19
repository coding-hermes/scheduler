package database

// SCHED-GAP-1665 — the AGENT REGISTRY store.
//
// A tick is dispatched to a LANE; the lane's execution target decides whether
// that work runs locally (the shared Hermes gateway) or is handed to a NAMED
// agent over the Crier bus (docs/dispatch-spec.md, internal/scheduler/
// dispatch_targets.go). The target file answers the routing question. This
// store answers the INVENTORY question an operator actually asks: which agents
// exist, what can each one do, and where is it reachable.
//
// THREE LAWS THIS STORE KEEPS:
//
//	ADVISORY, NEVER A GATE. An unregistered agent is still a valid dispatch
//	target: the relay's 404 is the authority on whether an agent exists
//	(dispatch-spec.md §4). Nothing here can make an existing remote lane
//	unroutable, which is what keeps the registry additive.
//
//	PERSISTED, NOT CACHED. Rows live in the scheduler database (migration
//	v71), so a registration survives a restart — the whole point of an
//	inventory that outlives one process.
//
//	ONE SHAPE. Capabilities are a normalized comma list and endpoints are
//	validated absolute http(s) URLs HERE, at the write path, so the API and
//	the MCP surface can never disagree about what a stored row means.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrAgentNotFound is returned when an agent lookup targets a name that is not
// registered.
var ErrAgentNotFound = errors.New("agent not found")

// MaxAgentNameLen bounds an agent name. The value is generous relative to the
// fleet's ids (a container hostname plus a project slug) while still keeping a
// pathological name out of an audit row.
const MaxAgentNameLen = 128

// Agent is one registered execution target: who it is, what it can do, and
// where it is reachable.
type Agent struct {
	// Name is the bus identity work is handed to (the registry key). It is
	// matched case-sensitively, exactly like the relay's agent id.
	Name string `json:"name"`
	// Capabilities is the normalized comma list (see NormalizeCapabilities):
	// "control, dispatch, query". Free-form vocabulary — the scheduler does
	// not interpret it beyond token matching; it exists so a policy can ask
	// "which agents declare X".
	Capabilities string `json:"capabilities"`
	// Endpoint is the agent's declared bus relay base URL
	// (e.g. "http://10.0.0.7:8767"). Empty = inherit the scheduler's
	// configured [crier] url, the same relay every lane uses today.
	Endpoint string `json:"endpoint"`
	// RegisteredAt / UpdatedAt are RFC3339 UTC bookkeeping stamps.
	RegisteredAt string `json:"registered_at"`
	UpdatedAt    string `json:"updated_at"`
}

// ValidateAgentName rejects a name that cannot be a bus identity. The charset
// is deliberately narrow: the name is interpolated into a relay path
// (/agents/{id}/inbox) and into audit rows, so spaces, path separators and
// control characters must never reach either.
func ValidateAgentName(name string) error {
	n := strings.TrimSpace(name)
	if n == "" {
		return errors.New("agent name is required")
	}
	if n != name {
		return fmt.Errorf("agent name %q has leading or trailing whitespace", name)
	}
	if len(n) > MaxAgentNameLen {
		return fmt.Errorf("agent name is %d bytes, want <= %d", len(n), MaxAgentNameLen)
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("agent name %q contains %q — allowed: letters, digits, '.', '_' and '-'", name, r)
		}
	}
	return nil
}

// ValidateAgentEndpoint accepts "" (inherit the scheduler's relay) or an
// absolute http(s) URL with a host. A relative path or a bare host:port is
// refused: an endpoint that cannot be addressed is worse than no endpoint,
// because it would look like routing information.
func ValidateAgentEndpoint(endpoint string) error {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return nil
	}
	u, err := url.Parse(e)
	if err != nil {
		return fmt.Errorf("agent endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("agent endpoint %q must be an absolute http(s) URL", endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("agent endpoint %q has no host", endpoint)
	}
	return nil
}

// NormalizeCapabilities canonicalizes a free-form capability list: split on
// commas, trim, drop empties, de-duplicate case-insensitively (first spelling
// wins) and join with ", ". Normalizing on the WRITE path is what lets the read
// side match a token with a plain comparison instead of guessing at spacing.
func NormalizeCapabilities(raw string) string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		key := strings.ToLower(token)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, token)
	}
	return strings.Join(out, ", ")
}

// AgentHasCapability reports whether the stored capability list declares want.
// Matching is token-wise and case-insensitive: "dispatch" matches
// "control, dispatch" but never "dispatcher".
func AgentHasCapability(capabilities, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	for _, token := range strings.Split(capabilities, ",") {
		if strings.ToLower(strings.TrimSpace(token)) == want {
			return true
		}
	}
	return false
}

// NormalizeAgent validates and canonicalizes an agent record in place
// (name echoed as given, capabilities normalized, endpoint trimmed). It is the
// single write-path gate both the REST and MCP surfaces call.
func NormalizeAgent(a *Agent) error {
	if err := ValidateAgentName(a.Name); err != nil {
		return err
	}
	if err := ValidateAgentEndpoint(a.Endpoint); err != nil {
		return err
	}
	a.Capabilities = NormalizeCapabilities(a.Capabilities)
	a.Endpoint = strings.TrimSpace(a.Endpoint)
	return nil
}

// UpsertAgent registers or refreshes an agent. The second return value reports
// whether the row was CREATED (true) or updated (false), so the API can answer
// 201 vs 200 honestly.
//
// The update arm deliberately preserves registered_at: re-registering an agent
// refreshes its advertised capabilities and endpoint, it does not rewrite its
// history. updated_at always moves.
func UpsertAgent(ctx context.Context, db *sql.DB, a *Agent) (bool, error) {
	if db == nil {
		return false, errors.New("upsert agent: nil database")
	}
	if err := NormalizeAgent(a); err != nil {
		return false, err
	}
	now := nowUTC(ctx)

	var existing string
	err := db.QueryRowContext(ctx, `SELECT registered_at FROM agents WHERE name = ?`, a.Name).Scan(&existing)
	switch {
	case err == nil:
		const q = `
UPDATE agents SET capabilities = ?, endpoint = ?, updated_at = ?
WHERE name = ?`
		if _, err := db.ExecContext(ctx, q, a.Capabilities, a.Endpoint, now, a.Name); err != nil {
			return false, fmt.Errorf("update agent %q: %w", a.Name, err)
		}
		a.RegisteredAt = existing
		a.UpdatedAt = now
		return false, nil
	case errors.Is(err, sql.ErrNoRows):
		const q = `
INSERT INTO agents (name, capabilities, endpoint, registered_at, updated_at)
VALUES (?, ?, ?, ?, ?)`
		if _, err := db.ExecContext(ctx, q, a.Name, a.Capabilities, a.Endpoint, now, now); err != nil {
			return false, fmt.Errorf("insert agent %q: %w", a.Name, err)
		}
		a.RegisteredAt = now
		a.UpdatedAt = now
		return true, nil
	default:
		return false, fmt.Errorf("lookup agent %q: %w", a.Name, err)
	}
}

// GetAgent loads one agent by name. Returns ErrAgentNotFound when absent, so a
// caller can distinguish "not registered" from a store failure.
func GetAgent(ctx context.Context, db *sql.DB, name string) (*Agent, error) {
	if db == nil {
		return nil, errors.New("get agent: nil database")
	}
	row := db.QueryRowContext(ctx,
		`SELECT name, capabilities, endpoint, registered_at, updated_at FROM agents WHERE name = ?`, name)
	a := &Agent{}
	if err := row.Scan(&a.Name, &a.Capabilities, &a.Endpoint, &a.RegisteredAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAgentNotFound
		}
		return nil, fmt.Errorf("get agent %q: %w", name, err)
	}
	return a, nil
}

// ListAgents returns the registered agents ordered by name (stable for the list
// surface). A non-empty capability filter keeps only agents declaring that
// token (see AgentHasCapability); an empty filter returns every row.
func ListAgents(ctx context.Context, db *sql.DB, capability string) ([]Agent, error) {
	if db == nil {
		return nil, errors.New("list agents: nil database")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT name, capabilities, endpoint, registered_at, updated_at FROM agents ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Agent{}
	want := strings.TrimSpace(capability)
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Name, &a.Capabilities, &a.Endpoint, &a.RegisteredAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan agent row: %w", err)
		}
		if want != "" && !AgentHasCapability(a.Capabilities, want) {
			continue
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent rows: %w", err)
	}
	return out, nil
}

// DeleteAgent unregisters an agent. An unknown name returns ErrAgentNotFound —
// a delete that removed nothing must never look like a success.
func DeleteAgent(ctx context.Context, db *sql.DB, name string) error {
	if db == nil {
		return errors.New("delete agent: nil database")
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("delete agent: name is required")
	}
	res, err := db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete agent %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete agent rows affected %q: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrAgentNotFound, name)
	}
	return nil
}
