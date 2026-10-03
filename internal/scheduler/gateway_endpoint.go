package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
)

// SCHED-GAP-1712 — per-lane and per-namespace gateway ENDPOINTS.
//
// The daemon used to resolve exactly ONE gateway for the whole fleet:
// config.GatewayConfig carries a single URL, and the project row carried only
// a gateway_key. Lanes could therefore be DISTINGUISHED (their own key) but
// never ADDRESSED (their own endpoint) — one Hermes gateway served every
// project in the fleet.
//
// This file adds the two missing tiers. Every tick resolves its endpoint at
// DISPATCH time through three levels, independently per field:
//
//	lane (projects.gateway_url | projects.gateway_key)
//	  ↓ when empty
//	namespace (namespaces.gateway_url | namespaces.gateway_key)
//	  ↓ when empty
//	global ([gateway].url | --gateway-key)
//
// A fleet that sets nothing resolves to the global endpoint on every tick,
// exactly as before this landed, and the resolved fact is recorded on the
// tick row (gateway_url / gateway_source / gateway_key_source) so a dispatch
// can be audited after the fact without replaying config.

// GatewayEndpoint is the RESOLVED gateway destination for one tick: the URL
// the dispatch is addressed to, the credential it authenticates with, and the
// tier each of those came from. URL and Key resolve independently — a lane
// may take its URL from its namespace while still using the shared global
// key. Source is always paired with its value: a non-empty URL always carries
// a non-empty source, and an empty URL always carries an empty source (a
// daemon with no gateway at all, or an exec spawn) — never a fabricated
// "global" label for an endpoint that does not exist.
type GatewayEndpoint struct {
	URL       string
	Key       string
	URLSource string // database.GatewaySourceLane | Namespace | Global | ""
	KeySource string // database.GatewaySourceLane | Namespace | Global | ""
}

// GatewayEndpointConfig is the raw material of the resolution: the three
// tiers, unmerged. Lane* is the lane's own entry (projects row), Namespace*
// its namespace's, Global* the daemon's. Any tier may be empty.
type GatewayEndpointConfig struct {
	LaneURL      string
	NamespaceURL string
	GlobalURL    string

	LaneKey      string
	NamespaceKey string
	GlobalKey    string
}

// ResolveGatewayEndpoint applies the lane > namespace > global precedence,
// per field, over the three tiers. Blank (or whitespace-only) values are
// "unset": a tier that holds only spaces never shadows the tier below it.
//
// This is a pure function so the precedence is unit-testable in isolation
// from the DB and the spawner, and so the API/dashboard can resolve an
// effective endpoint for display without dispatching anything.
func ResolveGatewayEndpoint(c GatewayEndpointConfig) GatewayEndpoint {
	ep := GatewayEndpoint{}
	ep.URL, ep.URLSource = resolveTier(c.LaneURL, c.NamespaceURL, c.GlobalURL)
	ep.Key, ep.KeySource = resolveTier(c.LaneKey, c.NamespaceKey, c.GlobalKey)
	return ep
}

// resolveTier picks the first non-blank tier, highest first, and names it.
// The returned source is "" when every tier is blank — the honest empty: there
// is no endpoint at this level, not a global one with no URL.
func resolveTier(lane, namespace, global string) (value, source string) {
	if v := strings.TrimSpace(lane); v != "" {
		return v, gatewaySourceLane
	}
	if v := strings.TrimSpace(namespace); v != "" {
		return v, gatewaySourceNamespace
	}
	if v := strings.TrimSpace(global); v != "" {
		return v, gatewaySourceGlobal
	}
	return "", ""
}

// Source literals. The database package owns the same three strings for the
// tick columns (database.GatewaySource*); these aliases keep the scheduler
// side free of a database import in the pure resolver while a test pins the
// two spellings together.
const (
	gatewaySourceLane      = "lane"
	gatewaySourceNamespace = "namespace"
	gatewaySourceGlobal    = "global"
)

// resolveTickEndpoint resolves ONE tick's endpoint from the DB row for the
// project, merged with the spawner's global gateway.
//
// The lane key is passed in rather than re-read: PackedProject.GatewayKey is
// already the resolved lane credential on every spawn path (packer, manual,
// resume), and keeping it as the input preserves the pre-1712 behavior
// byte-for-byte for a project whose row predates the column.
//
// A lookup failure is NEVER fatal to the tick: the tier simply reads as empty
// and resolution falls through to the global endpoint — the pre-1712
// behavior. ErrNoRows is the normal shape for spawns whose project row is
// absent (tests, one-off tooling), so it is not logged; any other error is
// logged once per tick as a warning.
func (s *Spawner) resolveTickEndpoint(ctx context.Context, projectName, laneKey string) GatewayEndpoint {
	var laneURL, nsURL, nsKey string
	if s.db != nil {
		err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(p.gateway_url, ''), COALESCE(ns.gateway_url, ''), COALESCE(ns.gateway_key, '')
FROM projects p LEFT JOIN namespaces ns ON ns.id = p.namespace_id
WHERE p.name = ?`, projectName).Scan(&laneURL, &nsURL, &nsKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("WARN: gateway endpoint lookup for %s failed (%v) — resolving against the global gateway only", projectName, err)
		}
	}
	return ResolveGatewayEndpoint(GatewayEndpointConfig{
		LaneURL:      laneURL,
		NamespaceURL: nsURL,
		GlobalURL:    s.gatewayURL,
		LaneKey:      laneKey,
		NamespaceKey: nsKey,
		GlobalKey:    s.gatewayKey,
	})
}

// endpointClient returns the GatewayClient for a resolved endpoint URL.
//
//   - An empty URL, or the daemon's own URL, returns the daemon client
//     installed by SetGatewayClient — so a fleet with no per-lane endpoints
//     uses the SAME client object, health gate and reconnect wiring as
//     before this landed (byte-identical dispatch path).
//   - Any other URL gets a derived client, created once and cached for the
//     daemon's lifetime: same tick timeout, same clock seam, but an EMPTY
//     fallback key — a foreign endpoint is never handed the daemon's shared
//     key implicitly. The resolved per-endpoint key is passed explicitly on
//     every request, which is also what keeps the credential tier auditable.
//
// A nil daemon client (exec-fallback mode) returns nil: the caller's existing
// gateway-nil guards own that path.
func (s *Spawner) endpointClient(url string) *GatewayClient {
	if s.gateway == nil {
		return nil
	}
	if url == "" || url == s.gateway.baseURL {
		return s.gateway
	}
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	if c, ok := s.gatewayEndpoints[url]; ok {
		return c
	}
	c := NewGatewayClient(url, "", s.gateway.timeout)
	c.SetClock(s.clk.Get())
	if s.gatewayEndpoints == nil {
		s.gatewayEndpoints = make(map[string]*GatewayClient)
	}
	s.gatewayEndpoints[url] = c
	log.Printf("GATEWAY: derived endpoint client for %s (cache size=%d)", url, len(s.gatewayEndpoints))
	return c
}

// endpointCount reports how many derived (non-global) endpoint clients this
// spawner has created. Diagnostics only.
func (s *Spawner) endpointCount() int {
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
	return len(s.gatewayEndpoints)
}

// recordTickEndpoint stamps the RESOLVED endpoint on the tick row at dispatch
// time. Best-effort by contract: this is observability — a failed stamp is
// logged and never blocks or fails the dispatch.
func (s *Spawner) recordTickEndpoint(ctx context.Context, tickID string, ep GatewayEndpoint) {
	if s.db == nil || tickID == "" {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE ticks SET gateway_url = ?, gateway_source = ?, gateway_key_source = ? WHERE id = ?`,
		ep.URL, ep.URLSource, ep.KeySource, tickID); err != nil {
		log.Printf("WARN: record gateway endpoint for tick %s: %v", tickID, err)
	}
}
