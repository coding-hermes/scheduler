package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// REMOTE-003 (§2 peer registry): the per-scheduler LOCAL registry of known
// federation peers. One row per peer, keyed by peer id; the registry lives in
// this scheduler's own database (no shared store, no broker — spec §2: the
// registry is local, per scheduler).
//
// Rendering law (docs/remote-spec.md §2): a peer with no heartbeat within the
// freshness window is STALE — with its last_contact timestamp — NEVER "down".
// There is no "down" state anywhere in this surface: freshness is computed at
// read time (IsPeerStale over the caller's window) and rendered as a boolean
// next to the raw last_contact string, which is always present (possibly ""
// when the peer never heartbeat).

// PeerFreshnessWindowDefault is the DEFAULT freshness window: a peer whose
// last heartbeat is older than this renders stale=true. Named per the spec's
// "freshness window = a named constant" contract; an operator flag layer can
// override it (see cmd/schedulerd/main.go --peer-freshness-window).
const PeerFreshnessWindowDefault = 3 * 60 // seconds

// ErrPeerNotFound is returned when a peer lookup targets an id that is not
// registered.
var ErrPeerNotFound = errors.New("peer not found")

// Peer is one registered federation peer: who it is, where its API lives,
// what it can do, and when it was last heard from.
type Peer struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	Version      string `json:"version"`
	Capabilities string `json:"capabilities"` // free-form capability string (e.g. "control,query"); stored verbatim
	// LastContact is the RFC3339 UTC instant of the peer's most recent
	// heartbeat — "" when the peer was registered but never heartbeat.
	LastContact string `json:"last_contact"`
	// RegisteredAt / UpdatedAt are RFC3339 UTC bookkeeping stamps.
	RegisteredAt string `json:"registered_at"`
	UpdatedAt    string `json:"updated_at"`
}

// UpsertPeer registers or refreshes a peer (POST /api/v1/peers). The update
// arm deliberately does NOT touch last_contact: registration refreshes the
// peer's identity, only a heartbeat refreshes its liveness.
func UpsertPeer(ctx context.Context, db *sql.DB, p *Peer) error {
	if strings.TrimSpace(p.ID) == "" {
		return errors.New("upsert peer: id must not be empty")
	}
	if p.LastContact == "" {
		p.LastContact = "" // never fabricated: registration is not liveness
	}
	now := nowUTC(ctx)
	const q = `
INSERT INTO peers (id, url, version, capabilities, last_contact, registered_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    url          = excluded.url,
    version      = excluded.version,
    capabilities = excluded.capabilities,
    updated_at   = excluded.updated_at
`
	_, err := db.ExecContext(ctx, q,
		p.ID, p.URL, p.Version, p.Capabilities, p.LastContact, now, now)
	if err != nil {
		return fmt.Errorf("upsert peer %q: %w", p.ID, err)
	}
	return nil
}

// GetPeer loads one peer by id. Returns ErrPeerNotFound when absent.
func GetPeer(ctx context.Context, db *sql.DB, id string) (*Peer, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, url, version, capabilities, COALESCE(last_contact, ''), registered_at, updated_at FROM peers WHERE id = ?`, id)
	p := &Peer{}
	if err := row.Scan(&p.ID, &p.URL, &p.Version, &p.Capabilities, &p.LastContact, &p.RegisteredAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPeerNotFound
		}
		return nil, fmt.Errorf("get peer %q: %w", id, err)
	}
	return p, nil
}

// ListPeers returns every registered peer ordered by id (stable for the list
// surface; freshness/staleness is the caller's render-time computation).
func ListPeers(ctx context.Context, db *sql.DB) ([]Peer, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, url, version, capabilities, COALESCE(last_contact, ''), registered_at, updated_at FROM peers ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	defer rows.Close()
	out := []Peer{}
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.ID, &p.URL, &p.Version, &p.Capabilities, &p.LastContact, &p.RegisteredAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan peer row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate peer rows: %w", err)
	}
	return out, nil
}

// PeerHeartbeat stamps a peer's last_contact to now (POST
// /api/v1/peers/{id}/heartbeat). The id does NOT auto-register: heartbeat is
// a liveness stamp on a REGISTERED peer — an unknown id reports
// ErrPeerNotFound so a misconfigured peer surfaces at its own registration
// step instead of silently existing (the API layer maps it to 404).
func PeerHeartbeat(ctx context.Context, db *sql.DB, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("peer heartbeat: id must not be empty")
	}
	res, err := db.ExecContext(ctx,
		`UPDATE peers SET last_contact = ?, updated_at = ? WHERE id = ?`, nowUTC(ctx), nowUTC(ctx), id)
	if err != nil {
		return fmt.Errorf("peer heartbeat %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("peer heartbeat rows affected %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrPeerNotFound, id)
	}
	return nil
}

// IsPeerStale is THE freshness predicate (one definition shared by every
// surface): a peer is stale when it has never heartbeat ("" or an
// unparseable timestamp — never a crash, always the honest "no signal") or
// when its last heartbeat is older than windowSeconds before now. The window
// is applied by the CALLER so the flag layer and tests drive it; the API
// handler passes the resolved --peer-freshness-window.
func IsPeerStale(lastContact string, windowSeconds int, now func() time.Time) bool {
	if strings.TrimSpace(lastContact) == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, lastContact)
	if err != nil {
		return true
	}
	return now().Sub(t) > time.Duration(windowSeconds)*time.Second
}
