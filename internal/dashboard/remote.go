package dashboard

// REMOTE-006 (docs/remote-spec.md §5 + §2 rendering law): the dashboard's
// Remote section. Every registered peer with its LAST-SEEN EVENT (kind /
// status / timestamp) and a staleness indicator that reuses the registry's
// stale-window semantics verbatim (IsPeerStale) — a peer that has not
// heartbeated within the window renders STALE with its last_contact
// timestamp, NEVER "down": there is no down state anywhere in this surface.
// htmx polls return the remote_rows fragment only (the /remote page swaps
// it innerHTML on autorefresh, exactly like the fleet table).

import (
	"context"
	"io"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

// RemoteData is the Remote section's render payload.
type RemoteData struct {
	Title       string
	GeneratedAt string
	// Peers is every registered peer with its last-seen event + freshness.
	Peers []database.RemotePeerLastEvent
	// DedupeDrops is the fleet-wide COUNTED dedupe total: duplicate keys the
	// durable ingest dropped (replays + re-deliveries), never errors.
	DedupeDrops int
	// DataError is set when the store read failed: the section then renders
	// an explicit unavailable notice instead of an empty table presented as
	// a complete fleet view (spec §8: never a partial list as complete).
	DataError string
}

// RemoteFreshnessWindowDefault is the dashboard's freshness window when no
// explicit one is installed: the registry's own default, re-used so the
// Remote section and the /api/v1/peers surface can never disagree about
// what "stale" means at the same instant.
const RemoteFreshnessWindowDefault = database.PeerFreshnessWindowDefault

// SetPeerFreshnessWindow overrides the staleness window (seconds). The
// daemon wires it from --peer-freshness-window so one flag governs every
// peer-freshness surface. Zero/negative keeps the default.
func (g *Generator) SetPeerFreshnessWindow(seconds int) {
	if seconds > 0 {
		g.peerFreshnessWindow = seconds
	}
}

// remoteData assembles the Remote section's facts; shared by the full page
// and the htmx rows fragment.
func (g *Generator) remoteData() RemoteData {
	ctx := context.Background()
	data := RemoteData{
		Title:       "Remote",
		GeneratedAt: g.clock().Now().UTC().Format(time.RFC3339),
	}
	window := g.peerFreshnessWindow
	if window <= 0 {
		window = RemoteFreshnessWindowDefault
	}
	peers, err := database.RemotePeersWithLastEvent(ctx, g.db, window, g.clock().Now)
	if err != nil {
		// The section renders the explicit unavailable notice rather than
		// failing the whole page — and never an empty table presented as a
		// complete fleet view (spec §8).
		data.DataError = err.Error()
		data.Peers = nil
	} else {
		data.Peers = peers
	}
	if drops, err := database.CountRemoteEventDedupeDrops(ctx, g.db, ""); err == nil {
		data.DedupeDrops = drops
	}
	return data
}

// GenerateRemote renders the full /remote page.
func (g *Generator) GenerateRemote(w io.Writer) error {
	return g.remoteTmpl.Execute(w, g.remoteData())
}

// GenerateRemoteRows renders only the peer rows fragment for htmx polling.
// The page's tbody polls /remote/partial with hx-swap=innerHTML, so the
// response must be the fragment — a full page swapped into its own poller
// compounds itself on every refresh (the /health contract).
func (g *Generator) GenerateRemoteRows(w io.Writer) error {
	return g.remoteTmpl.ExecuteTemplate(w, "remote_rows", g.remoteData())
}

// peerStatusClass maps an event status to the pill class the tick tables
// use (statusClass's semantics, scoped for compile-time safety in the
// remote template's func map — see loadTemplates).
func peerStatusClass(status string) string {
	switch status {
	case "completed":
		return "ok"
	case "failed":
		return "fail"
	case "timeout", "deferred":
		return "warn"
	default:
		return "info"
	}
}
