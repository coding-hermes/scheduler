package scheduler

import (
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/coding-hermes/scheduler/internal/database"
)

// SCHED-GAP-124: admission modes decide HOW a project becomes eligible for
// selection, replacing the one-size wall-clock gate:
//
//	"cooldown" (default, everywhere unchanged) — cron semantics. A tick is
//	admitted only when now - last_tick_completed >= effective cooldown.
//	"tasks" — work driven. A project whose board holds non-perpetual
//	pending work admits IMMEDIATELY (urgency/priority ordering, namespace
//	concurrency caps, weight budget, blackout and failure backoff all
//	still apply). When the board is drained — including the case where
//	only NEVER-DONE / perpetual fixture rows remain, which GAP-106
//	excludes from the count — the wall-clock cooldown pin applies again.
//
// The mode is pure config: namespaces carry the default, projects may
// override, and both are settable through the API, fleet.toml, and the CLI.
// Nothing here hardcodes a project or namespace name.
//
// SCHED-GAP-141: the tasks-mode waiver additionally requires BOARD
// OWNERSHIP. Measured leak (2026-09-17, live): the "satellite" lanes
// (-sync / -qa / -dogfood / -pm) run in workdirs whose board path is
// linked into ANOTHER project's workdir, so the walk resolved to the
// primary's tasks.jsonl; the lane read the PRIMARY's backlog as its own
// "has work" signal and never fell back to its cooldown pin — 22 -sync
// lanes produced 156 ticks in 24h against a 6h pin (4/day max). Bane's
// intent (2026-09-17): tasks mode is "fast when there is work, slow when
// it is only perpetual stuff"; a lane that merely READS someone else's
// board is a time-based lane and must follow its cooldown timer.
//
// The ownership rule is derived, never named: after full symlink
// resolution, the board file a lane reads must live inside that lane's own
// workdir. A lane whose board resolves outside its workdir (a symlink or
// bind-shared board, as every satellite lane ships) is "time-based" — the
// waiver is refused and the wall-clock cooldown decides, exactly as if the
// lane were in cooldown mode. urgency/priority ordering, namespace
// concurrency caps, budget caps, the pacing floor, blackout, failure
// backoff and the adaptive-cooldown pass are all untouched: the change
// only removes the cooldown WAIVER from lanes that do not own their board.
//
// Config coverage (no name heuristics anywhere):
//   - per-project admission_mode ("cooldown" | "tasks", already shipped)
//     remains the sanctioned per-lane knob and still wins over the
//     namespace default; pinning "cooldown" on ANY lane is honored
//     verbatim.
//   - per-project board_ownership (SCHED-GAP-141: "" = auto, "owner",
//     "shared") is the explicit override for the two cases the filesystem
//     walk cannot decide by itself: a lane whose board genuinely lives
//     outside its workdir but owns it ("owner"), and a lane that shares a
//     *copy* of another project's board where the path check would pass
//     ("shared"). Both are validated, API-settable, pinned from
//     fleet.toml by the loader, and re-pinned on restart like cooldowns.
//
// Known scope boundary: the post-tick adaptive-cooldown pass
// (adaptive_cooldown.go) still keys its escalation branch off the MODE
// (namespace/project), not off ownership — a tasks-namespace lane keeps its
// floor-pin cooldown. That is deliberate: ownership gates the waiver, it
// does not hand a time-based lane an escalating cooldown it never asked
// for. Satellites on a 6h pin stay 6h; they simply no longer skip it.

// Board ownership values (SCHED-GAP-141). Auto (the empty string) is the
// default for every existing row and the only value that needs no operator
// input; the explicit values exist so a fleet that does not match the
// default filesystem shape is configurable rather than special-cased in
// code.
const (
	// BoardOwnershipAuto derives ownership from the filesystem walk: the
	// board a lane reads must resolve inside that lane's own workdir.
	BoardOwnershipAuto = ""
	// BoardOwnershipOwner asserts the lane owns the board it reads, even
	// when the resolved board path lies outside the lane's workdir
	// (unusual layouts: generated boards, shared board directories).
	BoardOwnershipOwner = "owner"
	// BoardOwnershipShared asserts the lane reads a board it does NOT own
	// (paced by cooldown) even when the path check would pass — the
	// explicit escape hatch for a copied or bind-mounted foreign board.
	BoardOwnershipShared = "shared"
)

// admissionModeFor resolves the effective admission mode for a project:
// the per-project override wins, otherwise the namespace default. An
// unknown namespace id (or empty mode on both levels) resolves to
// cooldown semantics — the historical behavior.
func admissionModeFor(projectMode, namespaceID string, nsModes map[string]string) string {
	switch projectMode {
	case database.AdmissionModeCooldown, database.AdmissionModeTasks:
		return projectMode
	}
	if m, ok := nsModes[namespaceID]; ok && m != "" {
		return m
	}
	return database.AdmissionModeCooldown
}

// admissionModeForProject resolves the effective admission mode straight
// from the DB (project override → namespace default → cooldown). Used by
// post-tick paths (adaptive cooldown) that don't have the packer maps.
func admissionModeForProject(db *sql.DB, project string) string {
	var pm, nm string
	err := db.QueryRow(`SELECT COALESCE(p.admission_mode, ''), COALESCE(ns.admission_mode, '')
		FROM projects p LEFT JOIN namespaces ns ON ns.id = p.namespace_id
		WHERE p.name = ?`, project).Scan(&pm, &nm)
	if err != nil {
		return database.AdmissionModeCooldown
	}
	switch pm {
	case database.AdmissionModeCooldown, database.AdmissionModeTasks:
		return pm
	}
	switch nm {
	case database.AdmissionModeTasks:
		return database.AdmissionModeTasks
	default:
		return database.AdmissionModeCooldown
	}
}

// validBoardOwnership reports whether v is a settable board_ownership value
// ("" = auto, "owner", "shared"). Shared by the DB write path, the config
// loader and the CLI so all three agree on what is accepted.
func validBoardOwnership(v string) bool {
	switch v {
	case BoardOwnershipAuto, BoardOwnershipOwner, BoardOwnershipShared:
		return true
	}
	return false
}

// laneOwnsBoard reports whether the board a lane reads at workdir is the
// lane's OWN board (SCHED-GAP-141). Ownership is derived, never named: the
// board file found by the standard board walk is fully symlink-resolved and
// must live inside the lane's own (also resolved) workdir. A board reached
// through a link into another project's workdir — the satellite-lane shape —
// is not owned by this lane.
//
// Fail-closed: an empty workdir, a missing board, a dangling link or an
// unresolvable path all report "not owned", which refuses the cooldown
// waiver. Refusing the waiver only costs latency (the lane runs on its
// cooldown pin); granting it wrongly is the leak this exists to stop.
func laneOwnsBoard(workdir string) bool {
	if workdir == "" {
		return false
	}
	boardPath, hasBoard := findBoardFile(workdir)
	if !hasBoard {
		return false
	}
	resolvedBoard, err := filepath.EvalSymlinks(boardPath)
	if err != nil {
		return false
	}
	resolvedWorkdir, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedWorkdir, resolvedBoard)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// boardOwnedByLane resolves the effective ownership for a lane, honoring the
// explicit per-project override (SCHED-GAP-141) and falling back to the
// filesystem derivation. Unknown/empty values mean auto — a hand-edited row
// never silently changes semantics.
func boardOwnedByLane(workdir, ownership string) bool {
	switch ownership {
	case BoardOwnershipOwner:
		return true
	case BoardOwnershipShared:
		return false
	}
	return laneOwnsBoard(workdir)
}

// ownershipRefusal is the transition-deduped observability state: the eval
// loop runs every ~30s, so the refusal line is logged once per lane per
// ownership transition instead of on every evaluation.
var (
	ownershipRefusalMu   sync.Mutex
	ownershipRefusalSeen = map[string]bool{}
)

// noteOwnershipRefusal logs the refusal once per transition (refused →
// granted → refused), so operators can see WHY a tasks-mode lane is running
// on its cooldown pin: grep-able "ADMISSION-OWNERSHIP:" line.
func noteOwnershipRefusal(workdir string) {
	ownershipRefusalMu.Lock()
	prev := ownershipRefusalSeen[workdir]
	ownershipRefusalSeen[workdir] = true
	ownershipRefusalMu.Unlock()
	if !prev {
		log.Printf("ADMISSION-OWNERSHIP: <%s> board resolves outside its own workdir → time-based pacing (cooldown pin decides; SCHED-GAP-141)", workdir)
	}
}

// tasksAdmissionDue reports whether a tasks-mode project has admissible
// work RIGHT NOW (auto ownership — the default for every existing row). It
// uses the GAP-105/106 open-row scanner (shared with adaptive cooldown and
// the pending boost) so the definition of "work" is identical across every
// consumer: pending/open/in-progress vocabulary, malformed rows count open,
// perpetual fixtures excluded.
//
// Fail-open: when the board cannot be read, the scanner reports ok=false
// and we return false — the project falls back to its cooldown pin rather
// than being admitted on an unreadable signal (mirror of the packers'
// fail-open doctrine for the pending boost).
func tasksAdmissionDue(workdir string) bool {
	return tasksAdmissionDueWith(workdir, BoardOwnershipAuto)
}

// tasksAdmissionDueWith is tasksAdmissionDue with the per-project
// board_ownership override applied (SCHED-GAP-141). The waiver requires
// board OWNERSHIP: a lane that only reads another project's board is a
// time-based lane, so it falls back to its cooldown timer even while the
// board it reads is full of work.
func tasksAdmissionDueWith(workdir, ownership string) bool {
	if workdir == "" {
		return false
	}
	if !boardOwnedByLane(workdir, ownership) {
		noteOwnershipRefusal(workdir)
		return false
	}
	open, ok := boardOpenRows(workdir)
	if !ok {
		return false
	}
	return open > 0
}
