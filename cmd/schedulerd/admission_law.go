package main

import (
	"context"
	"database/sql"
	"log"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// applyAdmissionLaw is the SCHED-GAP-1696 boot-time correction: any lane whose
// LANE-LEVEL admission_mode contradicts its class (foreman=tasks, everything
// else=cooldown) is corrected to the class value, one ADMISSION-LAW line per
// lane naming the lane, the old value and the new one. A clean fleet logs
// ZERO such lines. The class derivation is the SAME one the /api/v1/status
// count and the API boundary check use (internal/scheduler.LaneClass), so the
// three surfaces cannot disagree.
//
// Only a NON-EMPTY lane-level contradiction is rewritten; an empty (inherit)
// lane-level value is left to its namespace, which the status count reports
// but boot does not silently mutate.
func applyAdmissionLaw(ctx context.Context, db *sql.DB) {
	projects, err := database.ListProjects(ctx, db, false)
	if err != nil {
		log.Printf("WARN: admission-law boot pass: list projects: %v", err)
		return
	}
	all := make(map[string]bool, len(projects))
	for _, p := range projects {
		all[p.Name] = true
	}
	for _, p := range projects {
		cls := scheduler.LaneClass(p.Name, p.Parent, scheduler.LaneOwnsSatellite(p.Name, all))
		want := scheduler.ExpectedAdmission(cls)
		if p.AdmissionMode == "" || p.AdmissionMode == want {
			continue
		}
		if err := database.UpdateProject(ctx, db, p.Name, database.ProjectUpdates{AdmissionMode: &want}); err != nil {
			log.Printf("WARN: admission-law boot pass: correct %s: %v", p.Name, err)
			continue
		}
		log.Printf("ADMISSION-LAW: lane %s (class=%s) admission_mode %q corrected to %q", p.Name, cls, p.AdmissionMode, want)
	}
}
