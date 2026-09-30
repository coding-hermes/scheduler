package api

import (
	"net/http"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
)

const cadenceWindow = 7 * 24 * time.Hour

type cadenceItem struct {
	Name                string   `json:"name"`
	TargetRunsPerDay    *float64 `json:"target_runs_per_day"`
	TargetSource        string   `json:"target_source"`
	AchievedRunsPerDay  float64  `json:"achieved_runs_per_day"`
	WindowDays          int      `json:"window_days"`
	TargetDeficitPerDay float64  `json:"target_deficit_per_day"`
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

	items := make([]cadenceItem, 0, len(projects))
	for i := range projects {
		p := projects[i]
		target, source, configured := database.EffectiveCadenceTarget(p)
		var targetPtr *float64
		deficit := 0.0
		if configured {
			targetPtr = &target
			deficit = target - rates[p.Name]
			if deficit < 0 {
				deficit = 0
			}
		}
		items = append(items, cadenceItem{
			Name:                p.Name,
			TargetRunsPerDay:    targetPtr,
			TargetSource:        source,
			AchievedRunsPerDay:  rates[p.Name],
			WindowDays:          int(cadenceWindow.Hours() / 24),
			TargetDeficitPerDay: deficit,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"cadence": items})
}
