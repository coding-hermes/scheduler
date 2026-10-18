package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SCHED-GAP-1700 — the lane relationship map.
//
// `projects.parent` (SCHED-GAP-1586) is the SINGLE-parent nesting shape the
// tree views resolve: one parent per lane, cycle-checked, and the source of
// the /api/v1/lanes/tree hierarchy. This file is a different relation —
// lane_links, a MANY-TO-MANY association map: a lane may carry several
// parents and several children at once, and either endpoint may live in the
// same namespace or in a different one.
//
// LAW: a lane link is REFERENCE/LOOKUP ONLY. Nothing in internal/scheduler
// reads this table. Writing or deleting a link must leave every lane row —
// admission_mode, cooldown_s, weight, priority, enabled, the cooldown pins —
// byte-identical, because the map answers "who is related to whom", not "who
// runs next". The tests assert that invariance directly (row snapshot before
// and after a link write).
//
// ENDPOINTS ARE NOT FOREIGN KEYS, matching projects.parent: lane names are
// soft-deleted (enabled=0) and purgeable, so a purged endpoint must not
// cascade a delete or block a write. Writes validate BOTH endpoints against
// the projects table (ErrUnknownLane otherwise); a later purge leaves a
// dangling link that reads back verbatim.

// Lane-link error vocabulary. The API maps each to a distinct status + code
// so a caller can branch on the failure class instead of parsing prose.
var (
	// ErrUnknownLane — a link endpoint does not name an existing lane row.
	ErrUnknownLane = errors.New("unknown lane")
	// ErrLaneLinkSelf — parent and child are the same lane.
	ErrLaneLinkSelf = errors.New("lane link endpoints name the same lane")
	// ErrLaneLinkExists — the (parent, child) edge is already recorded.
	ErrLaneLinkExists = errors.New("lane link already exists")
	// ErrLaneLinkNotFound — the edge being deleted does not exist.
	ErrLaneLinkNotFound = errors.New("lane link not found")
	// ErrLaneLinkCycle — the edge would make a lane its own ancestor.
	ErrLaneLinkCycle = errors.New("lane link would create a cycle")
)

// maxLaneLinkHops bounds every traversal. The write path refuses cycles, so
// the graph is a DAG; the bound is a defensive guard for out-of-band data.
const maxLaneLinkHops = 4096

// LaneLink is one parent→child association between two lanes.
//
// ParentNamespace / ChildNamespace are reported LIVE (joined from projects)
// with the recorded at-link-time value as the fallback for a purged lane, so
// a lane moved between namespaces never reports a stale namespace.
// CrossNamespace is the derived "the two endpoints sit in different
// namespaces" reading — computed, never stored, so it cannot drift.
type LaneLink struct {
	ParentLane      string `json:"parent_lane"`
	ChildLane       string `json:"child_lane"`
	ParentNamespace string `json:"parent_namespace"`
	ChildNamespace  string `json:"child_namespace"`
	CrossNamespace  bool   `json:"cross_namespace"`
	Note            string `json:"note"`
	CreatedAt       string `json:"created_at"`
	CreatedBy       string `json:"created_by"`
}

// LaneLinkNode is one lane reached by a traversal (ancestors or descendants).
// Depth is the number of edges between the queried lane and this one (direct
// parent/child = 1). Via names the edge that first reached the node, so a
// multi-parent lane's path is legible instead of a bare name list.
type LaneLinkNode struct {
	Lane      string `json:"lane"`
	Namespace string `json:"namespace"`
	Depth     int    `json:"depth"`
	ViaParent string `json:"via_parent,omitempty"`
	ViaChild  string `json:"via_child,omitempty"`
}

// laneLinkSelect is the ONE read projection: the stored edge plus the LIVE
// namespace of each endpoint, falling back to the recorded namespace when the
// lane row is gone (purged → dangling link, still legible).
const laneLinkSelect = `
SELECT l.parent_lane, l.child_lane,
       CASE WHEN p.name IS NULL THEN l.namespace_parent ELSE COALESCE(p.namespace_id, '') END,
       CASE WHEN c.name IS NULL THEN l.namespace_child  ELSE COALESCE(c.namespace_id, '') END,
       l.note, l.created_at, l.created_by
FROM lane_links l
LEFT JOIN projects p ON p.name = l.parent_lane
LEFT JOIN projects c ON c.name = l.child_lane`

// scanLaneLink reads one row of laneLinkSelect.
func scanLaneLink(scan func(dest ...interface{}) error) (LaneLink, error) {
	var l LaneLink
	if err := scan(&l.ParentLane, &l.ChildLane, &l.ParentNamespace, &l.ChildNamespace,
		&l.Note, &l.CreatedAt, &l.CreatedBy); err != nil {
		return LaneLink{}, err
	}
	l.CrossNamespace = l.ParentNamespace != l.ChildNamespace
	return l, nil
}

// laneLinkQuery runs laneLinkSelect with an extra WHERE/ORDER clause and
// collects the rows. A nil slice is never returned — callers serialize [].
func laneLinkQuery(ctx context.Context, db *sql.DB, suffix string, args ...interface{}) ([]LaneLink, error) {
	rows, err := db.QueryContext(ctx, laneLinkSelect+suffix, args...)
	if err != nil {
		return nil, fmt.Errorf("list lane links: %w", err)
	}
	defer rows.Close()
	out := []LaneLink{}
	for rows.Next() {
		l, err := scanLaneLink(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan lane link row: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lane link rows: %w", err)
	}
	return out, nil
}

// ListLaneLinks returns every recorded edge, ordered by (parent, child) so the
// listing is deterministic regardless of insert order.
func ListLaneLinks(ctx context.Context, db *sql.DB) ([]LaneLink, error) {
	return laneLinkQuery(ctx, db, ` ORDER BY l.parent_lane ASC, l.child_lane ASC`)
}

// LaneParents returns the lanes that name lane as their child (the direct
// parents), ordered by parent name. Empty lane → the whole edge list filtered
// by nothing is NOT what this means: use ListLaneLinks for that.
func LaneParents(ctx context.Context, db *sql.DB, lane string) ([]LaneLink, error) {
	if strings.TrimSpace(lane) == "" {
		return nil, fmt.Errorf("list lane parents: lane must not be empty")
	}
	return laneLinkQuery(ctx, db, ` WHERE l.child_lane = ? ORDER BY l.parent_lane ASC`, lane)
}

// LaneChildren returns the lanes that name lane as their parent (the direct
// children), ordered by child name.
func LaneChildren(ctx context.Context, db *sql.DB, lane string) ([]LaneLink, error) {
	if strings.TrimSpace(lane) == "" {
		return nil, fmt.Errorf("list lane children: lane must not be empty")
	}
	return laneLinkQuery(ctx, db, ` WHERE l.parent_lane = ? ORDER BY l.child_lane ASC`, lane)
}

// ListLaneLinksTouching returns every edge with lane as EITHER endpoint — the
// "everything related to this lane" view, ordered by (parent, child).
func ListLaneLinksTouching(ctx context.Context, db *sql.DB, lane string) ([]LaneLink, error) {
	if strings.TrimSpace(lane) == "" {
		return nil, fmt.Errorf("list lane links for lane: lane must not be empty")
	}
	return laneLinkQuery(ctx, db,
		` WHERE l.parent_lane = ? OR l.child_lane = ? ORDER BY l.parent_lane ASC, l.child_lane ASC`,
		lane, lane)
}

// laneNamespace resolves a lane name to its namespace id ("" = unscheduled).
// Returns ErrUnknownLane when no projects row carries the name.
func laneNamespace(ctx context.Context, db *sql.DB, lane string) (string, error) {
	var ns sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT namespace_id FROM projects WHERE name = ?`, lane).Scan(&ns)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %q", ErrUnknownLane, lane)
	}
	if err != nil {
		return "", fmt.Errorf("resolve namespace of lane %q: %w", lane, err)
	}
	if !ns.Valid {
		return "", nil
	}
	return ns.String, nil
}

// CreateLaneLink records one parent→child edge.
//
// Validation, in order: both endpoints non-empty and distinct; both name real
// lanes (ErrUnknownLane); the edge does not close a cycle (ErrLaneLinkCycle);
// the edge is not already recorded (ErrLaneLinkExists). The namespaces are
// DERIVED from the lane rows — never caller-supplied — so the stored columns
// cannot lie about where an endpoint lived when the link was made.
//
// createdBy is the audit identity (the operator credential's classification);
// "" is accepted (unattributed write) and stored verbatim.
func CreateLaneLink(ctx context.Context, db *sql.DB, parent, child, note, createdBy string) (LaneLink, error) {
	parent = strings.TrimSpace(parent)
	child = strings.TrimSpace(child)
	if parent == "" {
		return LaneLink{}, fmt.Errorf("create lane link: parent_lane is required")
	}
	if child == "" {
		return LaneLink{}, fmt.Errorf("create lane link: child_lane is required")
	}
	if parent == child {
		return LaneLink{}, fmt.Errorf("%w: %q", ErrLaneLinkSelf, parent)
	}

	parentNS, err := laneNamespace(ctx, db, parent)
	if err != nil {
		return LaneLink{}, err
	}
	childNS, err := laneNamespace(ctx, db, child)
	if err != nil {
		return LaneLink{}, err
	}

	// A cycle exists iff the prospective CHILD is already an ancestor of the
	// prospective PARENT (adding parent→child would then close the loop).
	closes, _, err := IsLaneAncestor(ctx, db, child, parent)
	if err != nil {
		return LaneLink{}, err
	}
	if closes {
		return LaneLink{}, fmt.Errorf("%w: %q is already an ancestor of %q", ErrLaneLinkCycle, child, parent)
	}

	link := LaneLink{
		ParentLane:      parent,
		ChildLane:       child,
		ParentNamespace: parentNS,
		ChildNamespace:  childNS,
		CrossNamespace:  parentNS != childNS,
		Note:            note,
		CreatedAt:       nowUTC(ctx),
		CreatedBy:       createdBy,
	}
	res, err := db.ExecContext(ctx, `
INSERT INTO lane_links (parent_lane, child_lane, namespace_parent, namespace_child, note, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(parent_lane, child_lane) DO NOTHING`,
		link.ParentLane, link.ChildLane, link.ParentNamespace, link.ChildNamespace,
		link.Note, link.CreatedAt, link.CreatedBy)
	if err != nil {
		return LaneLink{}, fmt.Errorf("create lane link %q→%q: %w", parent, child, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return LaneLink{}, fmt.Errorf("create lane link %q→%q: %w", parent, child, err)
	}
	if affected == 0 {
		return LaneLink{}, fmt.Errorf("%w: %q→%q", ErrLaneLinkExists, parent, child)
	}
	return link, nil
}

// DeleteLaneLink removes one parent→child edge. Returns ErrLaneLinkNotFound
// when the edge is absent (a delete that changed nothing must not report
// success — the caller has to be able to tell "removed" from "never existed").
func DeleteLaneLink(ctx context.Context, db *sql.DB, parent, child string) error {
	parent = strings.TrimSpace(parent)
	child = strings.TrimSpace(child)
	if parent == "" || child == "" {
		return fmt.Errorf("delete lane link: parent_lane and child_lane are both required")
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM lane_links WHERE parent_lane = ? AND child_lane = ?`, parent, child)
	if err != nil {
		return fmt.Errorf("delete lane link %q→%q: %w", parent, child, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete lane link %q→%q: %w", parent, child, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %q→%q", ErrLaneLinkNotFound, parent, child)
	}
	return nil
}

// laneNeighbours returns the lanes one hop from lane: the parents (direction
// "up") or the children (direction "down"), as lane-link nodes at depth 1.
func laneNeighbours(ctx context.Context, db *sql.DB, lane, direction string) ([]LaneLinkNode, error) {
	var (
		links []LaneLink
		err   error
	)
	if direction == "up" {
		links, err = LaneParents(ctx, db, lane)
	} else {
		links, err = LaneChildren(ctx, db, lane)
	}
	if err != nil {
		return nil, err
	}
	out := make([]LaneLinkNode, 0, len(links))
	for _, l := range links {
		if direction == "up" {
			out = append(out, LaneLinkNode{Lane: l.ParentLane, Namespace: l.ParentNamespace, Depth: 1, ViaParent: l.ParentLane, ViaChild: l.ChildLane})
		} else {
			out = append(out, LaneLinkNode{Lane: l.ChildLane, Namespace: l.ChildNamespace, Depth: 1, ViaParent: l.ParentLane, ViaChild: l.ChildLane})
		}
	}
	return out, nil
}

// LaneAncestorNodes walks UP from lane: every lane that can reach it through
// the link graph, nearest hop first (depth 1 = direct parent), each lane
// appearing once (the first — shortest — path wins). Deterministic: within a
// depth, lane names are ASC ordered.
func LaneAncestorNodes(ctx context.Context, db *sql.DB, lane string) ([]LaneLinkNode, error) {
	return walkLaneLinks(ctx, db, lane, "up")
}

// LaneDescendantNodes walks DOWN from lane: every lane reachable from it,
// nearest hop first (depth 1 = direct child), deduplicated, name-ASC within a
// depth.
func LaneDescendantNodes(ctx context.Context, db *sql.DB, lane string) ([]LaneLinkNode, error) {
	return walkLaneLinks(ctx, db, lane, "down")
}

// walkLaneLinks is the ONE traversal implementation both directions share.
// It is breadth-first (so Depth is the shortest hop count), cycle-safe (a
// seen set, plus the maxLaneLinkHops ceiling for out-of-band loops), and
// deterministic (each level is sorted by lane name before it is walked).
func walkLaneLinks(ctx context.Context, db *sql.DB, lane, direction string) ([]LaneLinkNode, error) {
	lane = strings.TrimSpace(lane)
	if lane == "" {
		return nil, fmt.Errorf("traverse lane links: lane must not be empty")
	}
	seen := map[string]bool{lane: true}
	out := []LaneLinkNode{}
	frontier := []string{lane}
	for depth := 1; depth <= maxLaneLinkHops && len(frontier) > 0; depth++ {
		next := []LaneLinkNode{}
		for _, cur := range frontier {
			neighbours, err := laneNeighbours(ctx, db, cur, direction)
			if err != nil {
				return nil, err
			}
			for _, n := range neighbours {
				if seen[n.Lane] {
					continue
				}
				seen[n.Lane] = true
				n.Depth = depth
				next = append(next, n)
			}
		}
		sort.Slice(next, func(i, j int) bool { return next[i].Lane < next[j].Lane })
		out = append(out, next...)
		frontier = frontier[:0]
		for _, n := range next {
			frontier = append(frontier, n.Lane)
		}
	}
	return out, nil
}

// IsLaneAncestor reports whether ancestor reaches descendant through the
// link graph, and returns the path (ancestor … descendant, inclusive) that
// proves it. This is the ONE predicate the cycle guard and the API's
// /api/v1/links/ancestor read share — never a per-surface reimplementation.
//
// Self is NOT an ancestor of itself (the empty path), which is what makes
// "is Y an ancestor of X" answer false for Y == X while the write path still
// refuses a self link separately.
func IsLaneAncestor(ctx context.Context, db *sql.DB, ancestor, descendant string) (bool, []string, error) {
	ancestor = strings.TrimSpace(ancestor)
	descendant = strings.TrimSpace(descendant)
	if ancestor == "" || descendant == "" {
		return false, nil, fmt.Errorf("lane ancestor check: both lane names are required")
	}
	if ancestor == descendant {
		return false, nil, nil
	}
	// BFS downward from ancestor, carrying the path so the first hit names a
	// concrete witness chain.
	type state struct {
		lane string
		path []string
	}
	seen := map[string]bool{ancestor: true}
	frontier := []state{{lane: ancestor, path: []string{ancestor}}}
	for depth := 0; depth < maxLaneLinkHops && len(frontier) > 0; depth++ {
		var next []state
		for _, cur := range frontier {
			links, err := LaneChildren(ctx, db, cur.lane)
			if err != nil {
				return false, nil, err
			}
			for _, l := range links {
				if l.ChildLane == descendant {
					path := append(append([]string{}, cur.path...), l.ChildLane)
					return true, path, nil
				}
				if seen[l.ChildLane] {
					continue
				}
				seen[l.ChildLane] = true
				path := append(append([]string{}, cur.path...), l.ChildLane)
				next = append(next, state{lane: l.ChildLane, path: path})
			}
		}
		frontier = next
	}
	return false, nil, nil
}
