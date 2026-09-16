package scheduler

import (
	"database/sql"

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

// tasksAdmissionDue reports whether a tasks-mode project has admissible
// work RIGHT NOW. It uses the GAP-105/106 open-row scanner (shared with
// adaptive cooldown and the pending boost) so the definition of "work" is
// identical across every consumer: pending/open/in-progress vocabulary,
// malformed rows count open, perpetual fixtures excluded.
//
// Fail-open: when the board cannot be read, the scanner reports ok=false
// and we return false — the project falls back to its cooldown pin rather
// than being admitted on an unreadable signal (mirror of the packers'
// fail-open doctrine for the pending boost).
func tasksAdmissionDue(workdir string) bool {
	if workdir == "" {
		return false
	}
	open, ok := boardOpenRows(workdir)
	if !ok {
		return false
	}
	return open > 0
}
