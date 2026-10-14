package api

// REMOTE-003 (§2): the federation peer registry surface.
//
//   - POST /api/v1/peers                 — register/refresh a peer (upsert)
//   - GET  /api/v1/peers                 — list peers with freshness
//   - POST /api/v1/peers/{id}/heartbeat  — liveness stamp
//
// Auth: every peers route is a MUTATION-or-federation surface — the spec
// fail-closes it behind the SAME operator credential as every other mutating
// route (SCHED-GAP-1602 gate, requireOperator), including the GET: the peer
// list exposes the fleet's topology (peer urls), which is not public read
// material.
//
// RENDERING LAW (docs/remote-spec.md §2): a peer with no heartbeat within the
// freshness window is STALE — with its last_contact timestamp — NEVER "down".
// stale is a boolean; last_contact is always present (possibly "" when the
// peer never heartbeat). There is no "down" state anywhere in this surface.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// peerFreshnessWindow is the freshness window applied at render time. Zero
// (a Server that never received SetPeerFreshnessWindow — tests, bare
// servers) falls back to database.PeerFreshnessWindowDefault. main.go
// resolves it once at boot from --peer-freshness-window.
func (s *Server) peerFreshnessWindow() int {
	if s.peerWindow > 0 {
		return s.peerWindow
	}
	return database.PeerFreshnessWindowDefault
}

// SetPeerFreshnessWindow installs the freshness window in seconds (immutable
// after boot — written once by main.go before the HTTP server starts).
// Non-positive values are ignored so a mis-set flag can never collapse the
// window to zero (which would render every peer stale immediately).
func (s *Server) SetPeerFreshnessWindow(seconds int) {
	if seconds > 0 {
		s.peerWindow = seconds
	}
}

// peerUpsertRequest is the POST /api/v1/peers body: {id, url, version,
// capabilities} (REMOTE-003 §2). id is required; the rest may be empty.
type peerUpsertRequest struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	Version      string `json:"version"`
	Capabilities string `json:"capabilities"`
}

// peerView is one entry in the GET /api/v1/peers list — exactly the
// documented shape {id, url, last_contact, stale, version}. Rendering law:
// stale is computed at read time; last_contact is always present (""
// = never heartbeated). NO "down" field exists — do not add one.
type peerView struct {
	ID          string `json:"id"`
	URL         string `json:"url"`
	LastContact string `json:"last_contact"`
	Stale       bool   `json:"stale"`
	Version     string `json:"version"`
}

// handlePeers routes the /api/v1/peers collection (GET list, POST upsert).
func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listPeers(w, r)
	case http.MethodPost:
		s.upsertPeer(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

// upsertPeer implements POST /api/v1/peers — register/refresh a peer → 200.
// SCHED-GAP-1602: mutation — identity required (fail-closed 503 when no
// operator credential is configured).
func (s *Server) upsertPeer(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r, "-") {
		return
	}
	var req peerUpsertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, 400, "id is required")
		return
	}
	p := &database.Peer{
		ID:           strings.TrimSpace(req.ID),
		URL:          req.URL,
		Version:      req.Version,
		Capabilities: req.Capabilities,
	}
	// Preserve the live last_contact across a re-registration: registration
	// refreshes identity, only a heartbeat refreshes liveness. UpsertPeer
	// leaves the column untouched on the update arm; reading the existing
	// row keeps the in-memory struct honest for the echo below.
	if existing, err := database.GetPeer(r.Context(), s.db, p.ID); err == nil {
		p.LastContact = existing.LastContact
	}
	if err := database.UpsertPeer(r.Context(), s.db, p); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"status":  "registered",
		"id":      p.ID,
		"url":     p.URL,
		"version": p.Version,
	})
}

// listPeers implements GET /api/v1/peers — every registered peer rendered
// through the freshness predicate. An empty registry returns [] (never null).
func (s *Server) listPeers(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r, "-") {
		return
	}
	peers, err := database.ListPeers(r.Context(), s.db)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	nowFn := s.clock().Now
	window := s.peerFreshnessWindow()
	out := make([]peerView, 0, len(peers))
	for _, p := range peers {
		out = append(out, peerView{
			ID:          p.ID,
			URL:         p.URL,
			LastContact: p.LastContact,
			Stale:       database.IsPeerStale(p.LastContact, window, nowFn),
			Version:     p.Version,
		})
	}
	writeJSON(w, 200, map[string]interface{}{"peers": out, "count": len(out)})
}

// handlePeerByID routes the /api/v1/peers/{id} subtree — today only the
// heartbeat sub-route; the plain {id} detail path is reserved (404) so a
// mistyped heartbeat surfaces instead of silently matching.
func (s *Server) handlePeerByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/peers/")
	if strings.HasSuffix(rest, "/heartbeat") {
		s.peerHeartbeat(w, r)
		return
	}
	// REMOTE-006: the durable per-peer event slice — the last-seen event,
	// the replay high-water mark, and the counted dedupe drops.
	if rest == "events" || strings.HasSuffix(rest, "/events") {
		if r.Method != http.MethodGet {
			writeError(w, 405, "GET only")
			return
		}
		if !s.requireOperator(w, r, "-") {
			return
		}
		s.peerEvents(w, r, strings.TrimSuffix(rest, "/events"))
		return
	}
	writeError(w, 404, "unknown peers sub-route")
}

// peerEvents serves GET /api/v1/peers/events (every scheduler) and
// GET /api/v1/peers/{id}/events (one scheduler): the durable per-peer
// event slice — last-seen event, replay high-water mark, counted dedupe
// drops. Read-only federation state; never mutates a peer (§4).
func (s *Server) peerEvents(w http.ResponseWriter, r *http.Request, peerID string) {
	ctx := r.Context()
	if peerID != "" {
		if _, err := database.GetPeer(ctx, s.db, peerID); err != nil {
			if errors.Is(err, database.ErrPeerNotFound) {
				writeError(w, 404, "unknown peer: "+peerID)
				return
			}
			writeError(w, 500, err.Error())
			return
		}
	}
	var last *database.RemoteEvent
	if ev, err := database.RemoteLastEvent(ctx, s.db, peerID); err == nil {
		last = ev
	} else if peerID != "" && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 500, err.Error())
		return
	} else if peerID == "" && !errors.Is(err, sql.ErrNoRows) {
		writeError(w, 500, err.Error())
		return
	}
	mark, err := database.RemoteLastEventID(ctx, s.db, peerID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	drops, err := database.CountRemoteEventDedupeDrops(ctx, s.db, peerID)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	out := map[string]interface{}{
		"scheduler_id":  peerID,
		"last_event":    last,
		"last_event_id": mark,
		"dedupe_drops":  drops,
	}
	if peerID == "" {
		out["scheduler_id"] = "*"
	}
	writeJSON(w, 200, out)
}

// peerHeartbeat implements POST /api/v1/peers/{id}/heartbeat — stamps
// last_contact. An unknown id is a 404 (heartbeat does NOT auto-register;
// a misconfigured peer must surface at its registration step).
func (s *Server) peerHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "POST only")
		return
	}
	if !s.requireOperator(w, r, "-") {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/peers/"), "/heartbeat")
	if strings.TrimSpace(id) == "" {
		writeError(w, 404, "unknown peers sub-route")
		return
	}
	if err := database.PeerHeartbeat(r.Context(), s.db, id); err != nil {
		if errors.Is(err, database.ErrPeerNotFound) {
			writeError(w, 404, "unknown peer: "+id)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]interface{}{"status": "heartbeat", "id": id})
}
