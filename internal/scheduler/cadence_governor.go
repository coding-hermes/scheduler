package scheduler

// SCHED-GAP-1745 — the governor behind a cadence row. The REST cadence
// surface (internal/api/server_cadence.go) and its MCP mirror
// (internal/mcp/handlers_peers_events.go toolCadence) must never present a
// cooldown-derived target deficit as undelivered work on a lane whose
// admission is BOARD-driven: a tasks-mode lane with dispatchable board work
// waives its cooldown pin entirely (admission_mode.go tasksAdmissionDue), so
// `86400 / cooldown_pin_s` is not its delivery pace and `target − achieved`
// is not a shortfall — the board decides.
//
// This file resolves the governor with the SAME predicates the pick paths
// apply, in the package that owns them:
//   - the admission-mode chain (admissionModeFor): project override →
//     namespace default → cooldown — so a taskslane is recognized whether
//     its mode is a project override or a namespace default;
//   - the board-ownership waiver gate (boardOwnedByLane, SCHED-GAP-141): a
//     tasks-mode lane that does not OWN the board it reads is time-based —
//     the waiver is refused and the pin paces it — so it reports
//     cooldown_pin exactly like a cooldown-mode lane, fail-closed.
//
// Nothing here mutates state, reads ticks, or changes any admission
// behavior: it is the read-only label the cadence surfaces hang their
// numbers on.

import (
	"github.com/coding-hermes/scheduler/internal/database"
)

// CadenceGovernorFor resolves the governor token for one lane from its
// stored admission mode, its namespace's default mode and its board
// ownership override. nsModes maps namespace id → admission_mode (the same
// shape the packers build); a missing/empty entry resolves to the cooldown
// default. The board-ownership check walks the lane's workdir exactly as
// tasksAdmissionDue does, so a shared-board satellite is cooldown-paced
// here and on the pick path alike — and a lane with no board at all is
// paced too (the fail-closed default), because nothing board-driven can
// admit it.
func CadenceGovernorFor(p database.Project, nsModes map[string]string) string {
	nsID := ""
	if p.NamespaceID != nil {
		nsID = *p.NamespaceID
	}
	if admissionModeFor(p.AdmissionMode, nsID, nsModes) == database.AdmissionModeTasks &&
		boardOwnedByLane(p.Workdir, p.BoardOwnership) {
		return database.CadenceGovernorTasksBoard
	}
	return database.CadenceGovernorCooldown
}
