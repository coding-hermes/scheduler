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
// Design rationale, options weighed and rejected: docs/adr/002-board-ownership-gates-tasks-waiver.md.
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
//     namespace default; pinning "cooldown" on ANY lane — namespace-wide
//     or per lane — is honored verbatim.
//   - per-project board_ownership (SCHED-GAP-141: "" = auto, "owner",
//     "shared") is the explicit override for the two cases the filesystem
//     walk cannot decide by itself: a lane whose board genuinely lives
//     outside its workdir but owns it ("owner"), and a lane that shares a
//     *copy* of another project's board where the path check would pass
//     ("shared"). Both are validated, API-settable, pinned from
//     fleet.toml by the loader, and re-pinned on restart like cooldowns.
//
// Why realpath containment and NOT inode/hardlink checks (measured
// 2026-09-17): the satellite workdir ships a REAL .coding-hermes/ dir
// whose `board` entry is a SYMLINK to the primary's board dir. os.Stat
// follows that link, so owner and satellite report the SAME (dev, inode)
// and st_nlink stays 1 for both — inode identity cannot separate them and
// a hardlink count (nlink > 1) is never true. Fully resolving the path
// (filepath.EvalSymlinks on the board AND on the workdir) and requiring
// containment is the only discriminator that actually holds, and it needs
// no names, no suffixes and no project list.
//
// Known scope boundary: the post-tick adaptive-cooldown pass
// (adaptive_cooldown.go) still keys its escalation branch off the MODE
// (namespace/project), not off ownership — a tasks-namespace lane keeps its
// floor-pin cooldown. That is deliberate: ownership gates the waiver, it
// does not hand a time-based lane an escalating cooldown it never asked
// for. Satellites on a 6h pin stay 6h; they simply no longer skip it.

// The board-ownership values (SCHED-GAP-141) live in the database package
// (database.BoardOwnershipAuto/Owner/Shared) so the DB write path, the
// config loader and the scheduler all validate against one definition:
// "" = auto (derived from the board walk), "owner" = assert ownership,
// "shared" = assert a foreign board (always cooldown-paced).

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
	case database.BoardOwnershipOwner:
		return true
	case database.BoardOwnershipShared:
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

// tasksAdmissionDue reports whether a tasks-mode project has admissible work
// RIGHT NOW, with the per-project board_ownership override applied
// (SCHED-GAP-141; empty ownership = auto, the default for every existing
// row). It uses the GAP-105/106 open-row scanner (shared with adaptive
// cooldown and the pending boost) so the definition of "work" is identical
// across every consumer: pending/open/in-progress vocabulary, malformed rows
// count open, perpetual fixtures excluded.
//
// The waiver requires board OWNERSHIP: a lane that only reads another
// project's board is a time-based lane, so it falls back to its cooldown
// timer even while the board it reads is full of work.
//
// Fail-open: when the board cannot be read, the scanner reports ok=false
// and we return false — the project falls back to its cooldown pin rather
// than being admitted on an unreadable signal (mirror of the packers'
// fail-open doctrine for the pending boost).
func tasksAdmissionDue(workdir, ownership string) bool {
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

// lastTickStatusFailed reports whether the project's most recent tick FAILED
// (SCHED-GAP-214, migration v38 projects.last_tick_status). A failed tick
// stands the SCHED-GAP-124 tasks-mode cooldown waiver down: the next tick
// paces on the project's full effective cooldown instead of re-admitting on
// the 5s eval debounce.
//
// WHY THIS EXISTS — the measured defect. Every lane in the 2026-09-16 crier
// storm (91 DISTINCT failed ticks in ~17 minutes, 9s apart, all
// "gateway unreachable and exec fallback disabled") was a TASKS-mode lane:
// the waiver ignores the 21600s cooldown pin whenever open board work
// exists, and SCHED-GAP-143 deliberately leaves consecutive_failures at 0
// for a transport-class failure — so SCHED-GAP-133's backoff gate never
// fired either. Cooldown-mode lanes were already protected by their pin;
// the waiver was the one admission path with no post-failure spacing.
//
// Reading the status column (not re-deriving from the last tick row) keeps
// this O(1) on the admission path and makes the state auditable in SQL:
// SELECT name FROM projects WHERE last_tick_status='failed'.
//
// A legacy row (pre-v38) reads "" — treated as NOT failed, preserving the
// pre-214 waiver behavior until the project's next terminal tick stamps it.
func lastTickStatusFailed(lastStatus string) bool {
	return lastStatus == database.LastStatusFailed
}
