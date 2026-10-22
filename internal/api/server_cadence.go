package api

import (
	"net/http"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

const cadenceWindow = 7 * 24 * time.Hour

type cadenceItem struct {
	Name                string   `json:"name"`
	TargetRunsPerDay    *float64 `json:"target_runs_per_day"`
	TargetSource        string   `json:"target_source"`
	AchievedRunsPerDay  float64  `json:"achieved_runs_per_day"`
	WindowDays          int      `json:"window_days"`
	TargetDeficitPerDay float64  `json:"target_deficit_per_day"`
	// AdmissionGovernor names the governor behind the row (SCHED-GAP-1745):
	// "tasks_board" = admission is board-driven (the cooldown pin is waived
	// while dispatchable board work exists, admission_mode.go
	// tasksAdmissionDue), so no pin-derived target is claimed — the zero
	// deficit is a no-opinion zero, not measured delivery;
	// "cooldown_pin" = admission is paced by the wall-clock cooldown, so the
	// target and its deficit describe real expected delivery.
	AdmissionGovernor string `json:"admission_governor"`
}

// cadence serves configured-vs-achieved run rates without adding another
// aggregate to the latency-sensitive /api/v1/projects read path.
func (s *Server) cadence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, obs := s.newRequestDeadline(r.Context(), "cadence", s.readTimeout())
	defer obs.finish()

	obs.enter("ListProjects")
	projects, err := database.ListProjects(ctx, s.db, false)
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	obs.enter("LoadCadenceRates")
	rates, err := database.LoadCadenceRates(ctx, s.db, s.clock().Now(), cadenceWindow)
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// SCHED-GAP-1745: resolve the namespace admission-mode defaults once so
	// every row can name the governor that actually paced it — the same
	// resolution chain the scheduler's admissionModeFor applies (project
	// override → namespace default → cooldown).
	obs.enter("ListNamespaces")
	namespaces, err := database.ListNamespaces(ctx, s.db, false)
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	nsModes := make(map[string]string, len(namespaces))
	for _, ns := range namespaces {
		nsModes[ns.ID] = ns.AdmissionMode
	}

	items := make([]cadenceItem, 0, len(projects))
	for i := range projects {
		p := projects[i]
		target, source, configured := database.EffectiveCadenceTarget(p)
		governor := scheduler.CadenceGovernorFor(p, nsModes)
		var targetPtr *float64
		deficit := 0.0
		// A tasks-admission lane is board-driven: the cooldown pin is
		// WAIVED whenever dispatchable work exists (admission_mode.go
		// tasksAdmissionDue), so a pin-derived target is not its delivery
		// pace and the deficit would present board backpressure as
		// undelivered work. The governor context replaces it — explicitly,
		// never by reinterpreting the numbers a cooldown row serves.
		if governor == database.CadenceGovernorCooldown {
			if configured {
				targetPtr = &target
				deficit = target - rates[p.Name]
				if deficit < 0 {
					deficit = 0
				}
			}
		} else if source == "cooldown_pin" {
			// The only configured target was the pin: a board-driven lane
			// has no cadence opinion of its own.
			source = database.CadenceGovernorTasksBoard
		}
		items = append(items, cadenceItem{
			Name:                p.Name,
			TargetRunsPerDay:    targetPtr,
			TargetSource:        source,
			AchievedRunsPerDay:  rates[p.Name],
			WindowDays:          int(cadenceWindow.Hours() / 24),
			TargetDeficitPerDay: deficit,
			AdmissionGovernor:   governor,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"cadence": items})
}
