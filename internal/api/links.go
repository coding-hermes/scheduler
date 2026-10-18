package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-1700 — the lane relationship map.
//
// The `/api/v1/links` surface is the read/write face of lane_links: MANY-TO-
// MANY parent/child references between lanes, in one namespace or across
// namespaces. It answers the three lookup questions an operator actually
// asks, each in ONE call and with no name-suffix heuristics anywhere in the
// path:
//
//	who are X's parents      → GET /api/v1/links/parents?lane=X
//	who are X's children     → GET /api/v1/links/children?lane=X
//	is Y an ancestor of X    → GET /api/v1/links/ancestor?lane=X&ancestor=Y
//
// plus the transitive closures (ancestors/descendants) the same walk
// produces.
//
// REFERENCE/LOOKUP ONLY: these handlers touch lane_links and nothing else.
// No lane row is read for scheduling purposes and no scheduling field is
// written, so creating or deleting a link can never change admission,
// cooldown, weight, priority or pick order (SCHED-GAP-1700 acceptance 3).
//
// Mutations (POST/DELETE) go through requireOperator like every other
// mutating surface (SCHED-GAP-1602); reads are open like their siblings.

// linkError is the machine-readable failure class returned with every
// refusal. "Named error" means a caller can branch on `code` without parsing
// the message — cycle, unknown_lane, self_link, link_exists, link_not_found
// are distinct classes, not one generic 400.
type linkError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// writeLinkError writes the named-error envelope.
func writeLinkError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, linkError{Error: msg, Code: code})
}

// linkCreateRequest is the POST /api/v1/links body. snake_case per S06.
type linkCreateRequest struct {
	ParentLane string `json:"parent_lane"`
	ChildLane  string `json:"child_lane"`
	Note       string `json:"note"`
}

// laneLinkView is the wire shape of one edge. It mirrors database.LaneLink
// field for field on purpose: the storage type IS the contract (one shape,
// no per-surface copy to drift).
type laneLinkView = database.LaneLink

// handleLinks serves /api/v1/links:
//
//	GET     list every edge, optionally filtered (?lane=X, ?parent=X, ?child=X)
//	POST    record one edge (operator-gated)
//	DELETE  remove one edge (?parent=X&child=Y, operator-gated)
func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listLinks(w, r)
	case http.MethodPost:
		s.createLink(w, r)
	case http.MethodDelete:
		s.deleteLink(w, r)
	default:
		writeError(w, 405, "GET, POST or DELETE only")
	}
}

// listLinks implements GET /api/v1/links.
//
// Filters (all optional, ANDed): `lane` matches EITHER endpoint (the
// "everything related to this lane" view), `parent` restricts to edges whose
// parent is the named lane, `child` restricts to edges whose child is.
func (s *Server) listLinks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lane := strings.TrimSpace(q.Get("lane"))
	parent := strings.TrimSpace(q.Get("parent"))
	child := strings.TrimSpace(q.Get("child"))

	var (
		links []database.LaneLink
		err   error
	)
	switch {
	case lane != "":
		links, err = database.ListLaneLinksTouching(r.Context(), s.db, lane)
	case parent != "" && child != "":
		links, err = database.ListLaneLinks(r.Context(), s.db)
		if err == nil {
			links = filterLinks(links, parent, child)
		}
	case parent != "":
		links, err = database.LaneChildren(r.Context(), s.db, parent)
	case child != "":
		links, err = database.LaneParents(r.Context(), s.db, child)
	default:
		links, err = database.ListLaneLinks(r.Context(), s.db)
	}
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if links == nil {
		links = []database.LaneLink{}
	}
	writeJSON(w, 200, struct {
		Links  []laneLinkView `json:"links"`
		Count  int            `json:"count"`
		Lane   string         `json:"lane,omitempty"`
		Parent string         `json:"parent,omitempty"`
		Child  string         `json:"child,omitempty"`
	}{Links: links, Count: len(links), Lane: lane, Parent: parent, Child: child})
}

// filterLinks keeps the edges matching an exact (parent, child) pair.
func filterLinks(links []database.LaneLink, parent, child string) []database.LaneLink {
	out := make([]database.LaneLink, 0, len(links))
	for _, l := range links {
		if l.ParentLane == parent && l.ChildLane == child {
			out = append(out, l)
		}
	}
	return out
}

// createLink implements POST /api/v1/links — record one parent→child edge.
// 201 on success; the named error classes are mapped to their statuses:
// unknown_lane / self_link → 400, cycle / link_exists → 409.
func (s *Server) createLink(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r, "links") {
		return
	}
	var req linkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeLinkError(w, 400, "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	link, err := database.CreateLaneLink(r.Context(), s.db,
		req.ParentLane, req.ChildLane, req.Note, IdentityFromContext(r.Context()))
	if err != nil {
		writeLinkErrorFromDB(w, err)
		return
	}
	writeJSON(w, 201, struct {
		Link laneLinkView `json:"link"`
	}{Link: link})
}

// deleteLink implements DELETE /api/v1/links?parent=X&child=Y.
func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request) {
	if !s.requireOperator(w, r, "links") {
		return
	}
	q := r.URL.Query()
	parent := strings.TrimSpace(q.Get("parent"))
	child := strings.TrimSpace(q.Get("child"))
	if parent == "" || child == "" {
		writeLinkError(w, 400, "invalid_request",
			"parent_lane and child_lane are both required (DELETE /api/v1/links?parent=X&child=Y)")
		return
	}
	if err := database.DeleteLaneLink(r.Context(), s.db, parent, child); err != nil {
		writeLinkErrorFromDB(w, err)
		return
	}
	writeJSON(w, 200, map[string]interface{}{
		"status":      "deleted",
		"parent_lane": parent,
		"child_lane":  child,
	})
}

// writeLinkErrorFromDB maps the storage-layer error vocabulary onto HTTP
// statuses + named codes. Every branch keeps the underlying message (which
// names the offending lane) so the refusal is actionable.
func writeLinkErrorFromDB(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, database.ErrUnknownLane):
		writeLinkError(w, 404, "unknown_lane", err.Error())
	case errors.Is(err, database.ErrLaneLinkSelf):
		writeLinkError(w, 400, "self_link", err.Error())
	case errors.Is(err, database.ErrLaneLinkCycle):
		writeLinkError(w, 409, "cycle", err.Error())
	case errors.Is(err, database.ErrLaneLinkExists):
		writeLinkError(w, 409, "link_exists", err.Error())
	case errors.Is(err, database.ErrLaneLinkNotFound):
		writeLinkError(w, 404, "link_not_found", err.Error())
	default:
		writeError(w, 500, err.Error())
	}
}

// handleLinkSub serves the traversal sub-routes under /api/v1/links/.
//
//	GET /api/v1/links/parents?lane=X      direct parents
//	GET /api/v1/links/children?lane=X     direct children
//	GET /api/v1/links/ancestors?lane=X    transitive parents (nearest first)
//	GET /api/v1/links/descendants?lane=X  transitive children (nearest first)
//	GET /api/v1/links/ancestor?lane=X&ancestor=Y  yes/no + witness path
//
// Every one of these is a SINGLE call answering its question from the link
// graph — no client-side walk, no name-suffix inference.
func (s *Server) handleLinkSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "GET only")
		return
	}
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/links"), "/")
	if sub == "" {
		// "/api/v1/links/" with nothing after it — same surface as the exact
		// route, so the trailing-slash alias cannot 404 behind the operator's
		// back.
		s.listLinks(w, r)
		return
	}
	lane := strings.TrimSpace(r.URL.Query().Get("lane"))

	switch sub {
	case "parents":
		if !s.laneMustExist(w, r, lane) {
			return
		}
		links, err := database.LaneParents(r.Context(), s.db, lane)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if links == nil {
			links = []database.LaneLink{}
		}
		writeJSON(w, 200, struct {
			Lane    string         `json:"lane"`
			Parents []laneLinkView `json:"parents"`
			Count   int            `json:"count"`
		}{Lane: lane, Parents: links, Count: len(links)})

	case "children":
		if !s.laneMustExist(w, r, lane) {
			return
		}
		links, err := database.LaneChildren(r.Context(), s.db, lane)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if links == nil {
			links = []database.LaneLink{}
		}
		writeJSON(w, 200, struct {
			Lane     string         `json:"lane"`
			Children []laneLinkView `json:"children"`
			Count    int            `json:"count"`
		}{Lane: lane, Children: links, Count: len(links)})

	case "ancestors":
		if !s.laneMustExist(w, r, lane) {
			return
		}
		nodes, err := database.LaneAncestorNodes(r.Context(), s.db, lane)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, struct {
			Lane      string                  `json:"lane"`
			Ancestors []database.LaneLinkNode `json:"ancestors"`
			Count     int                     `json:"count"`
		}{Lane: lane, Ancestors: nodes, Count: len(nodes)})

	case "descendants":
		if !s.laneMustExist(w, r, lane) {
			return
		}
		nodes, err := database.LaneDescendantNodes(r.Context(), s.db, lane)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, struct {
			Lane        string                  `json:"lane"`
			Descendants []database.LaneLinkNode `json:"descendants"`
			Count       int                     `json:"count"`
		}{Lane: lane, Descendants: nodes, Count: len(nodes)})

	case "ancestor":
		if !s.laneMustExist(w, r, lane) {
			return
		}
		ancestor := strings.TrimSpace(r.URL.Query().Get("ancestor"))
		if ancestor == "" {
			writeLinkError(w, 400, "invalid_request",
				"the ancestor query parameter is required (GET /api/v1/links/ancestor?lane=X&ancestor=Y)")
			return
		}
		isAncestor, path, err := database.IsLaneAncestor(r.Context(), s.db, ancestor, lane)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if path == nil {
			path = []string{}
		}
		hops := 0
		if len(path) > 0 {
			hops = len(path) - 1
		}
		writeJSON(w, 200, struct {
			Lane       string   `json:"lane"`
			Ancestor   string   `json:"ancestor"`
			IsAncestor bool     `json:"is_ancestor"`
			Path       []string `json:"path"`
			Hops       int      `json:"hops"`
		}{Lane: lane, Ancestor: ancestor, IsAncestor: isAncestor, Path: path, Hops: hops})

	default:
		writeLinkError(w, 404, "unknown_sub_resource",
			"unknown lane-link sub-resource "+sub+
				" (want parents, children, ancestors, descendants or ancestor)")
	}
}

// laneMustExist writes the 400/404 refusal for a missing lane query parameter
// or an unknown lane, and reports whether the handler may continue.
//
// A traversal on an unknown lane would answer "nothing found", which is
// indistinguishable from a typo — so the subject lane must name a real lane
// row. Dangling links themselves stay readable through GET /api/v1/links
// ?lane=…, which does not require the lane row.
func (s *Server) laneMustExist(w http.ResponseWriter, r *http.Request, lane string) bool {
	if lane == "" {
		writeLinkError(w, 400, "invalid_request", "the lane query parameter is required")
		return false
	}
	if _, err := database.GetProject(r.Context(), s.db, lane); err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			writeLinkError(w, 404, "unknown_lane", "unknown lane: "+lane)
			return false
		}
		writeError(w, 500, err.Error())
		return false
	}
	return true
}
