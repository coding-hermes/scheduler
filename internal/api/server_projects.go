package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/database"
	"github.com/coding-hermes/scheduler/internal/scheduler"
)

// handleProjects handles GET (list) and POST (create).
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listProjects(w, r)
	case http.MethodPost:
		s.createProject(w, r)
	default:
		writeError(w, 405, "GET or POST only")
	}
}

// projectListItem is the /api/v1/projects list element: the full project row
// plus the SCHED-GAP-066 budget telemetry. Remaining* are nil (JSON null)
// when the corresponding cap is unlimited (<= 0); budget_blocked is true when
// any configured cap is reached, in which case blocked_reason is "budget" and
// budget_window names the exhausted window ("daily" / "weekly" / "final").
type projectListItem struct {
	database.Project
	SpentDailyUSD      float64  `json:"spent_daily_usd"`
	SpentWeeklyUSD     float64  `json:"spent_weekly_usd"`
	SpentTotalUSD      float64  `json:"spent_total_usd"`
	RemainingDailyUSD  *float64 `json:"remaining_daily_usd"`
	RemainingWeeklyUSD *float64 `json:"remaining_weekly_usd"`
	RemainingFinalUSD  *float64 `json:"remaining_final_usd"`
	BudgetBlocked      bool     `json:"budget_blocked"`
	BlockedReason      string   `json:"blocked_reason"`
	BudgetWindow       string   `json:"budget_window,omitempty"`
}

// budgetRemaining returns cap-spent clamped at 0, or nil when cap <= 0
// (unlimited budget → JSON null remaining).
func budgetRemaining(capUSD, spentUSD float64) *float64 {
	if capUSD <= 0 {
		return nil
	}
	r := capUSD - spentUSD
	if r < 0 {
		r = 0
	}
	return &r
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ctx := context.Background()
	projects, err := database.ListProjects(ctx, s.db, false)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if projects == nil {
		projects = []database.Project{}
	}
	// SCHED-GAP-066: enrich each project with its budget spend/remaining and
	// blocked state. Fail-open: if the spend query breaks, serve the plain
	// project rows rather than erroring the whole endpoint.
	spends, spendErr := scheduler.LoadBudgetSpends(ctx, s.db, time.Now())
	if spendErr != nil {
		writeJSON(w, 200, map[string]interface{}{"projects": projects})
		return
	}
	items := make([]projectListItem, 0, len(projects))
	for i := range projects {
		p := &projects[i]
		spend := spends[p.Name]
		window := scheduler.BudgetBlockReason(p, spend)
		item := projectListItem{
			Project:            *p,
			SpentDailyUSD:      spend.Daily,
			SpentWeeklyUSD:     spend.Weekly,
			SpentTotalUSD:      spend.Total,
			RemainingDailyUSD:  budgetRemaining(p.DailyBudgetUSD, spend.Daily),
			RemainingWeeklyUSD: budgetRemaining(p.WeeklyBudgetUSD, spend.Weekly),
			RemainingFinalUSD:  budgetRemaining(p.FinalBudgetUSD, spend.Total),
			BudgetBlocked:      window != "",
			BudgetWindow:       window,
		}
		if window != "" {
			item.BlockedReason = "budget"
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]interface{}{"projects": items})
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var p database.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if p.Name == "" || p.RepoURL == "" || p.Workdir == "" {
		writeError(w, 400, "name, repo_url, workdir are required")
		return
	}
	// Fill S06 defaults for zero-valued fields so a minimal {name, repo_url,
	// workdir} body satisfies the CHECK constraints. Enabled intentionally
	// stays false — creating a project must not auto-enable it.
	if p.Weight == 0 {
		p.Weight = 10
	}
	if p.Priority == 0 {
		p.Priority = 5
	}
	if p.CooldownS == 0 {
		p.CooldownS = 900
	}
	if p.DecayRate == 0 {
		p.DecayRate = 1.0
	}
	if err := database.CreateProject(context.Background(), s.db, &p); err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, 409, "project already exists")
			return
		}
		if strings.Contains(err.Error(), "already registered by enabled project") {
			writeError(w, 409, err.Error())
			return
		}
		if isCheckConstraint(err) {
			writeError(w, 400, projectConstraintMessage)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, p)
}

// isCheckConstraint reports whether err is a SQLite CHECK-constraint
// violation — i.e. a client-correctable value-range problem, not a server
// fault. Handlers map it to 400 with an actionable message.
func isCheckConstraint(err error) bool {
	return strings.Contains(err.Error(), "CHECK constraint failed")
}

// projectConstraintMessage is the actionable 400 body for projects-table
// CHECK violations (weight/priority/decay_rate ranges).
const projectConstraintMessage = "invalid project fields: weight must be 1..100; priority 1..10; decay_rate > 0"

// handleProjectByID handles GET, PUT, POST on /projects/:name and sub-routes.
func (s *Server) handleProjectByID(w http.ResponseWriter, r *http.Request) {
	// Strip the /api/v1/projects/ prefix to get the resource path.
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
	parts := splitPath(path)
	if len(parts) < 1 || parts[0] == "" {
		writeError(w, 400, "project name required")
		return
	}
	name := parts[0]

	// Sub-routes on /projects/:name.
	if len(parts) == 2 {
		if r.Method != http.MethodPost {
			writeError(w, 405, "POST only")
			return
		}
		switch parts[1] {
		case "pause":
			s.pauseProject(w, r, name)
			return
		case "resume":
			s.resumeProject(w, r, name)
			return
		case "spawn":
			s.spawnProject(w, r, name)
			return
		case "bump":
			s.bumpProject(w, r, name)
			return
		case "unbump":
			s.unbumpProject(w, r, name)
			return
		}
		writeError(w, 404, "not found")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getProject(w, r, name)
	case http.MethodPut:
		s.updateProject(w, r, name)
	case http.MethodDelete:
		s.deleteProject(w, r, name)
	default:
		writeError(w, 405, "GET, PUT, POST, or DELETE only")
	}
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	tick, _ := getLatestTick(ctx, s.db, name)
	writeJSON(w, 200, map[string]interface{}{
		"project":     p,
		"latest_tick": tick,
	})
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request, name string) {
	var updates database.ProjectUpdates
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	ctx := context.Background()
	// GAP-044: an enabled→disabled transition through PUT is a disable
	// path — the DB layer stamps provenance; the events table gets a
	// matching entry here.
	cur, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	wasEnabled := cur.Enabled
	// DecayRate guard: 0 makes urgency flat (priority × 1^0) so the project is
	// never picked by the packer — a silent permanent starvation. Foremen must
	// not be able to do this to themselves. Proven: dexdat-memory starved 87h
	// with a valid namespace + 900s cooldown because decay_rate was 0.
	if updates.DecayRate != nil && *updates.DecayRate <= 0 {
		writeError(w, 400, "decay_rate must be > 0 (0 causes permanent starvation — urgency never grows)")
		return
	}
	if err := database.UpdateProject(ctx, s.db, name, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		// SCHED-GAP-124: invalid admission_mode surfaces as 400.
		if strings.Contains(err.Error(), "invalid admission_mode") {
			writeError(w, 400, err.Error())
			return
		}
		if isCheckConstraint(err) {
			writeError(w, 400, projectConstraintMessage)
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// GAP-044: log a matching events-table entry when this PUT disabled
	// a previously-enabled project.
	if wasEnabled && updates.Enabled != nil && !*updates.Enabled {
		logDisableEvent(ctx, s.db, name, p.DisabledBy, p.DisabledReason, p.DisabledAt)
	}
	writeJSON(w, 200, p)
}

func (s *Server) pauseProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	// GAP-044: pause is a disable path — stamp explicit provenance so the
	// row and the events table both carry who/when/why.
	by := "api-pause"
	reason := "paused via POST /projects/{name}/pause"
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{
		Enabled:        database.BoolPtr(false),
		DisabledBy:     &by,
		DisabledReason: &reason,
	}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if p, err := database.GetProject(ctx, s.db, name); err == nil {
		logDisableEvent(ctx, s.db, name, p.DisabledBy, p.DisabledReason, p.DisabledAt)
	}
	// SCHED-GAP-137b: keep fleet.toml in parity with the DB. The loader
	// (internal/config/loader.go ApplyFleetConfig) re-pins enabled from
	// fleet.toml at every startup, so a pause that lives only in the DB is
	// silently undone on restart. Regenerate the toml via the official
	// policy script (SCHED-GAP-025: it is the ONLY writer of fleet.toml).
	// Log errors but never poison the API response: the policy script has
	// its own success criterion (the next policy run / --verify is the
	// backstop).
	if err := s.regenFleetTomlViaPolicy(); err != nil {
		log.Printf("SCHED-GAP-137b: pause %s: fleet.toml regen failed: %v", name, err)
	}
	writeJSON(w, 200, map[string]string{"status": "paused", "project": name})
}

func (s *Server) resumeProject(w http.ResponseWriter, r *http.Request, name string) {
	if err := database.UpdateProject(context.Background(), s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(true)}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// SCHED-GAP-137b: mirror pause — regenerate fleet.toml so the durable
	// pin matches the re-enabled DB row before the next daemon restart.
	if err := s.regenFleetTomlViaPolicy(); err != nil {
		log.Printf("SCHED-GAP-137b: resume %s: fleet.toml regen failed: %v", name, err)
	}
	writeJSON(w, 200, map[string]string{"status": "resumed", "project": name})
}

// regenFleetTomlViaPolicy regenerates ~/.hermes/fleet.toml from live DB state
// by running the ops policy script (SCHED-GAP-025: fleet-cooldown-policy.py
// is the ONLY writer of fleet.toml). SCHED-GAP-137b: pause/resume call this
// so the durable toml pin matches the DB and ApplyFleetConfig cannot
// silently re-enable a paused project on restart.
//
// regenFleetTomlExec is a package-level var so tests can inject a fake
// (the test must never touch the real fleet.toml). Any failure — script
// non-zero exit (stderr retained in the error) or exec helper failure — is
// returned to the caller, which logs it and proceeds: the response is never
// poisoned and the policy script's own --verify run is the backstop.
var regenFleetTomlExec = func() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", "/home/kara/.hermes/scripts/fleet-cooldown-policy.py", "--apply")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Non-zero exit is logged via the returned error, never an API
		// failure — the policy script owns its success criterion.
		return fmt.Errorf("policy script failed (stderr: %s): %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// regenFleetTomlViaPolicy invokes the injectable runner. Kept as a method so
// handlers read uniformly and tests can swap regenFleetTomlExec directly.
func (s *Server) regenFleetTomlViaPolicy() error {
	return regenFleetTomlExec()
}

// deleteProject removes a project. With only confirm=true it soft-deletes
// (sets enabled=false; the row is retained so historical ticks stay
// referentially valid). With confirm=true&purge=true it hard-deletes the row
// permanently (DOGFOOD-009) — historical ticks keep their project_name and
// /api/v1/status failure rates exclude projects whose row no longer exists.
// It requires an explicit confirm=true query param and refuses enabled
// projects so a live fleet project can never be silently disabled or
// removed by a stray DELETE.
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, name string) {
	q := r.URL.Query()
	purge := q.Get("purge") == "true"
	// Confirm flag checked first: even a valid, enabled project must not be
	// touched without explicit confirmation. Purge has its OWN confirm
	// requirement — ?purge=true alone is refused just like a bare DELETE.
	if q.Get("confirm") != "true" {
		writeError(w, 400, "confirm=true query param required — this soft-deletes the project (enabled=false); add purge=true to permanently remove the row")
		return
	}
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	// Enabled-project guard: deleting a live project would silently starve
	// it of ticks — require an explicit pause first.
	if p.Enabled {
		writeError(w, 409, "project is enabled — pause it first (PUT Enabled=false or POST /projects/{name}/pause) before deleting")
		return
	}
	if purge {
		if err := database.PurgeProject(ctx, s.db, name); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		// The row is gone, so a disable-provenance event makes no sense;
		// log a purge audit entry instead.
		_ = database.LogEvent(ctx, s.db, &database.Event{
			Severity:  database.SeverityInfo,
			Component: "api",
			Message:   fmt.Sprintf("project purged (hard delete): %s", name),
			Details:   `{"project":"` + name + `","action":"purge","via":"DELETE ?confirm=true&purge=true"}`,
		})
		writeJSON(w, 200, map[string]string{"status": "purged", "project": name})
		return
	}
	if err := database.DeleteProject(ctx, s.db, name); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// GAP-044: the DELETE soft-delete stamps provenance (api-delete,
	// legacy-backfilling COALESCE) — mirror it into the events table.
	if p, err := database.GetProject(ctx, s.db, name); err == nil {
		logDisableEvent(ctx, s.db, name, p.DisabledBy, p.DisabledReason, p.DisabledAt)
	}
	writeJSON(w, 200, map[string]string{"status": "deleted", "project": name})
}

// logDisableEvent records a GAP-044 disable-provenance entry in the events
// table so every API disable path (pause, PUT enabled=false, DELETE
// confirm=true) has a matching audit row with who/when/why.
func logDisableEvent(ctx context.Context, db *sql.DB, name, by, reason, at string) {
	details, _ := json.Marshal(map[string]string{
		"project":         name,
		"disabled_by":     by,
		"disabled_reason": reason,
		"disabled_at":     at,
	})
	_ = database.LogEvent(ctx, db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api",
		Message:   fmt.Sprintf("project disabled: %s (%s)", name, by),
		Details:   string(details),
	})
}

// spawnProject handles POST /api/v1/projects/:name/spawn.
//
// DOGFOOD-015: the returned tick_id is the REAL stored row id — generated by
// database.NextTickID (canonical UTC) and enqueued synchronously via
// Loop.SpawnNow — so the documented spawn → GET /ticks/{id} workflow always
// resolves. The old handler predicted an id with time.Now().UTC().Format
// while SlotPool.Spawn stamped the stored row with LOCAL time, so on a
// non-UTC host the returned id 404'd forever.
func (s *Server) spawnProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}
	tickID, err := s.loop.SpawnNow(*p)
	if err != nil {
		if errors.Is(err, scheduler.ErrProjectRunning) {
			writeError(w, 409, err.Error())
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{
		"status":  "spawned",
		"project": name,
		"tick_id": tickID,
	})
}

// bumpRequestBody is the JSON body for POST /api/v1/projects/{name}/bump
// (SCHED-GAP-107). Zero-valued fields take the defaults (ticks=5,
// cooldown=7200); reason is REQUIRED — an unattributed speed-up is
// unauditable.
type bumpRequestBody struct {
	Ticks    *int    `json:"ticks"`
	Cooldown *int    `json:"cooldown"`
	Reason   *string `json:"reason"`
}

// bumpProject handles POST /api/v1/projects/{name}/bump — temporarily
// accelerate the project to a small cooldown (>= the 7200s killer-lane
// floor) for at most 8 ticks, then auto-revert. Validation: reason
// non-empty (400), ticks 1..8 with default 5 (400), cooldown >= 7200 with
// default 7200 (400), project exists (404), project enabled (409 — a
// paused project cannot consume bump ticks), no active bump (409 — clear
// it first or let it expire).
func (s *Server) bumpProject(w http.ResponseWriter, r *http.Request, name string) {
	var body bumpRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	if !p.Enabled {
		writeError(w, 409, "project is disabled — resume it before bumping (a paused project cannot consume bump ticks)")
		return
	}
	if p.BumpActive {
		writeError(w, 409, fmt.Sprintf("bump already active (%d tick(s) remaining, reason %q) — let it expire or POST /projects/%s/unbump", p.BumpRemainingTicks, p.BumpReason, name))
		return
	}
	reason := ""
	if body.Reason != nil {
		reason = strings.TrimSpace(*body.Reason)
	}
	if reason == "" {
		writeError(w, 400, "reason is required — a bump is an auditable speed-up, name why")
		return
	}
	ticks := database.DefaultBumpTicks
	if body.Ticks != nil {
		ticks = *body.Ticks
	}
	if ticks < 1 || ticks > database.MaxBumpTicks {
		writeError(w, 400, fmt.Sprintf("ticks must be 1..%d (got %d)", database.MaxBumpTicks, ticks))
		return
	}
	cooldown := database.DefaultBumpCooldown
	if body.Cooldown != nil {
		cooldown = *body.Cooldown
	}
	if cooldown < database.MinBumpCooldown {
		writeError(w, 400, fmt.Sprintf("cooldown must be >= %ds — the 6h cooldown law floor applies to bumps too (got %d)", database.MinBumpCooldown, cooldown))
		return
	}
	updated, err := database.BumpProject(ctx, s.db, name, ticks, cooldown, reason)
	if err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	// Audit trail: every bump gets an events-table entry with who/why.
	details, _ := json.Marshal(map[string]any{
		"project":  name,
		"ticks":    ticks,
		"cooldown": cooldown,
		"reason":   reason,
		"saved": map[string]int{
			"cooldown_s":         updated.BumpSavedCooldownS,
			"cooldown_floor_s":   updated.BumpSavedFloorS,
			"cooldown_ceiling_s": updated.BumpSavedCeilingS,
			"no_progress_ticks":  updated.BumpSavedNoProgress,
		},
	})
	_ = database.LogEvent(ctx, s.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api",
		Message:   fmt.Sprintf("project bumped: %s (%d ticks @ %ds — %s)", name, ticks, cooldown, reason),
		Details:   string(details),
	})
	writeJSON(w, 200, updated)
}

// unbumpProject handles POST /api/v1/projects/{name}/unbump — manually
// abort an active bump. This is Phase A ONLY (restore the saved pre-bump
// state); no Phase B adaptive re-evaluation runs because an explicit cancel
// wants the pre-bump state verbatim, not a fresh verdict over whatever the
// last tick did.
func (s *Server) unbumpProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	if err := database.ClearBump(ctx, s.db, name); err != nil {
		if errors.Is(err, database.ErrProjectNotFound) {
			writeError(w, 404, "project not found")
			return
		}
		if errors.Is(err, database.ErrNoActiveBump) {
			writeError(w, 409, "no active bump to clear")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	_ = database.LogEvent(ctx, s.db, &database.Event{
		Severity:  database.SeverityInfo,
		Component: "api",
		Message:   "bump manually cleared: " + name,
		Details:   `{"project":"` + name + `","via":"POST /projects/` + name + `/unbump"}`,
	})
	writeJSON(w, 200, p)
}
