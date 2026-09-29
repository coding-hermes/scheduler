package scheduler

// SCHED-GAP-1660 — board writes no longer act on cooldowns (Bane ruling
// 2026-09-28, verbatim): "a cooldown shouldn't be effected by a board write
// anymore, the only time a board write effects a cooldown is the following.
// When the lane is under the effect of task addmision but because the board
// is Empty (excluding perpetual) it [is] now on cooldown mode. If an item is
// added to the board the temporary cooldown is now flipped back to task
// admission."
//
// The ruling's one allowed effect, and everything it excludes:
//
//  1. A COOLDOWN-mode lane is structurally unable to be admitted by a board
//     write. The board watcher stamps nudge_source=board_wake before forcing
//     evaluation; the wrapped hook installed by NewLoop drops that stamp
//     whenever the wake could admit nothing but a cooldown lane (the stamp
//     is what a wake-driven admission rides, so removing it removes the
//     effect — the packer's wall-clock gate refuses the lane on its own).
//     The pin itself is untouched: the lane still admits the moment it
//     expires, and the wake's evaluate() pass still runs for every OTHER
//     lane. The stamp's other producers keep their contract: the
//     orphan-resume scan (SCHED-GAP-101, "startup") and the operator's
//     SpawnNow ("manual") are not board writes; the wrapper intercepts only
//     the watcher's hook.
//
//  2. A tasks-admission lane whose board holds no non-perpetual open work is
//     PARKED-EMPTY — it runs on a temporary cooldown (the GAP-124 fallback
//     the mode already has). The park is recorded at the point the
//     evaluation already knows it (noteParkedEmpty, called from
//     classifyAdmissionDeferral's board read — one authority for the
//     predicate, tasksAdmissionDue/boardOpenRows). When a wake fires for a
//     parked-empty lane, the flip-back is allowed: the stamp goes through,
//     the packer waives the pin now that the board holds work, and the
//     resulting tick is stamped admit_reason=flip:board_empty by the slot
//     pool — WHICH CONSUMES THE PARK MARK to decide it. That consumption is
//     the scoping: only the lane that parked (and is therefore still
//     carrying the mark when its next admission is recorded) can be
//     labelled a flip. A lane that merely received the board-wake stamp —
//     any tasks lane on the fleet, since the stamp is process-wide and set
//     once per fired wake — is admitted as an ordinary nudge and keeps
//     resume:board_wake, so the flip count measures park-flips instead of
//     "a board write happened somewhere recently". A perpetual-only write
//     parks the lane again instead of flipping (the board still holds no
//     admissible work), so the packer's unchanged GAP-124/106 gate refuses
//     it.
//
//     The flipped tick still rides the rest of the gate stack: failure
//     backoff (GAP-133), post-tick pacing floor (GAP-136), the failed-tick
//     stand-down (GAP-214), namespace caps, budget, and the load gate. Only
//     the lane's own wall-clock pin is waived — that IS the flip.
//
//  3. admit_reason is stamped non-empty on EVERY tick row from every path:
//     nudge ticks keep resume:<source>, the flip ticks carry
//     flip:board_empty, packer ticks keep "ok", and sim-spawn rows are now
//     stamped too (the sim path never had a stamp — part of the 96 blank
//     rows in the 7-day sample).
import (
	"sync"

	"github.com/coding-hermes/scheduler/internal/database"
)

// AdmissionReasonFlipBoardEmpty is the admit_reason stamped on the tick a
// parked-empty flip admits (SCHED-GAP-1660). Grep-stable vocabulary, same
// register as AdmissionReasonOK / AdmissionReasonCooldown.
const AdmissionReasonFlipBoardEmpty = "flip:board_empty"

// parkedMu/parkedEmptySet is the parked-empty registry. An in-memory map is
// deliberately the whole mechanism (the brief's "keep it minimal"): the park
// is DERIVED from the board on every evaluation pass, so a column would
// duplicate the truth behind a migration, and the only consumer is this
// process's board-wake hook. After a restart the state re-derives on the
// first classification pass, so the restart cannot admit a lane whose
// cooldown has not expired (test 3) — the resume scan (SCHED-GAP-101) keeps
// its own, pre-existing semantics for genuinely orphaned rows.
var (
	parkedMu       sync.Mutex
	parkedEmptySet = map[string]bool{}
)

// noteParkedEmpty records that a tasks-admission lane's board currently holds
// no non-perpetual pending work — the park state, recorded at the point the
// evaluation already knows it. parked=false clears the mark: live work, a
// foreign board, or an unreadable board all mean "not parked" (the last two
// are the fail-open half of the GAP-141 ownership gate and the GAP-105/106
// scanner — the lane is cooldown-paced, not parked-empty, and must not flip).
func noteParkedEmpty(project string, parked bool) {
	parkedMu.Lock()
	defer parkedMu.Unlock()
	if parked {
		parkedEmptySet[project] = true
	} else {
		delete(parkedEmptySet, project)
	}
}

// isParkedEmpty reports whether the lane is recorded as parked-empty.
func isParkedEmpty(project string) bool {
	parkedMu.Lock()
	defer parkedMu.Unlock()
	return parkedEmptySet[project]
}

// consumeParkedEmpty reads AND CLEARS the lane's park mark, reporting whether
// it was set. This is the registry's one production reader: the slot pool
// calls it for every spawn of a lane and uses the answer to decide whether
// the admission it is recording IS the sanctioned park-flip (mark present)
// or an ordinary entry (mark absent — the board-wake stamp alone never
// authorises the flip label; SCHED-GAP-1660 deliverable (a)).
//
// Consuming rather than reading is what keeps the label one admission wide.
// The mark is set by a pass that DEFERS a tasks-admission lane on an empty
// board and cleared by noteParkedEmpty only when a later classification pass
// sees work — but an ADMITTED candidate is never classified (emitAdmissionPass
// classifies the candidates the packers did not select), so a lane that is
// admitted keeps a set mark. A read-only consumer would therefore stamp the
// lane's NEXT board-wake admission — long after the park ended — as a flip
// that never happened. Clearing at the first spawn bounds the mark to the
// exactly one admission that ended the park.
//
// A spawn that returns early (namespace cap, slot patience, load gate) never
// reaches the caller, so those paths leave the mark intact for the retry.
func consumeParkedEmpty(project string) bool {
	parkedMu.Lock()
	defer parkedMu.Unlock()
	parked, ok := parkedEmptySet[project]
	if ok {
		delete(parkedEmptySet, project)
	}
	return parked
}

// clearParkedEmpty removes every park mark. Boot hygiene: a restart must not
// inherit a park recorded before it, so NewLoop clears the registry.
func clearParkedEmpty() {
	parkedMu.Lock()
	defer parkedMu.Unlock()
	parkedEmptySet = map[string]bool{}
}

// wakeAdmitsAnyLane reports whether the wake that just fired may admit
// anything under the SCHED-GAP-1660 ruling: some ENABLED lane resolves to
// tasks-admission (SCHED-GAP-124). Only a fleet where every enabled lane is
// cooldown-mode — or where no lanes exist — returns false. Parked lanes are
// tasks-mode lanes by construction (noteParkedEmpty is only ever set from
// the tasks branch of classifyAdmissionDeferral), so the parked-empty flip
// is covered by the same predicate. Fail-open on the read: a DB error keeps
// the raw stamp (the packer's gates still decide — never worse than absent).
func wakeAdmitsAnyLane(l *Loop) bool {
	if l == nil || l.db == nil {
		return false
	}
	rows, err := l.db.Query(`SELECT admission_mode, COALESCE((SELECT admission_mode FROM namespaces ns WHERE ns.id = p.namespace_id), '') FROM projects p WHERE enabled = 1`)
	if err != nil {
		return true // fail-open: let the raw hook through, gates decide
	}
	defer rows.Close()
	for rows.Next() {
		var pm, nm string
		if err := rows.Scan(&pm, &nm); err != nil {
			continue
		}
		if admissionModeFor(pm, "", map[string]string{"": nm}) == database.AdmissionModeCooldown {
			continue // (c): no board write may touch a cooldown lane
		}
		return true // tasks lane: normal waiver, or the parked-empty flip
	}
	return false
}

// boardWakeAdmissionHook wraps the raw board-wake nudge hook (the closure
// NewLoop used to store in boardWakeNudgeSource, invoked once per fired wake
// before the forced evaluation). The wrapper is the single point every board
// write reaches the loop, so it implements the ruling there: the raw stamp
// (and therefore any wake-driven admission) only goes through when the wake
// can admit a tasks-admission lane. On a cooldown-only fleet the stamp is
// DROPPED — a board write is structurally unable to act on a cooldown —
// while the forced evaluate() still runs (harmless for cooldown lanes: the
// packer's wall-clock gate refuses them; every other lane treats the pass as
// normal).
func boardWakeAdmissionHook(l *Loop, raw func()) func() {
	return func() {
		if wakeAdmitsAnyLane(l) {
			raw()
		}
	}
}
