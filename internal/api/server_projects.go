package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
		// SCHED-GAP-1602: create is a mutation — identity required.
		if !s.requireOperator(w, r, "-") {
			return
		}
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
	// SCHED-GAP-1575-B: heavy read surface (the second-heaviest after
	// /api/v1/status) — request-scoped deadline so a stalled budget-spend
	// query answers 504 naming the helper instead of hanging.
	ctx, obs := s.newRequestDeadline(r.Context(), "projects", s.readTimeout())
	defer obs.finish()
	obs.enter("ListProjects")
	projects, err := database.ListProjects(ctx, s.db, false)
	if !obs.check(w, ctx) {
		return
	}
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
	//
	// SCHED-GAP-1636: the spends come from a short-TTL snapshot cache on this
	// read path (see budget_spend_cache.go) so a dashboard/picker poll no
	// longer re-runs the full ticks aggregate, and so the endpoint stops
	// occupying the daemon's single SQLite connection once per request —
	// which is what pushed it past the 5s handler budget.
	obs.enter("LoadBudgetSpends")
	spends, spendErr := s.loadBudgetSpends(ctx, s.clock().Now())
	if !obs.check(w, ctx) {
		return
	}
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
	// SCHED-GAP-150: a retired dagger-era driver must never be registered on a
	// project row — reject before any DB write. The Python fleet gate
	// (ops/check-fleet-invariants.py check #4) catches an already-enabled row;
	// this is the same list enforced at the write.
	if isRetiredCommand(p.Command) {
		writeError(w, 400, retiredCommandError(p.Command))
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
	// SCHED-GAP-1607: deliver_mode is stored as given — settable exactly the
	// way deliver is (free-form string; no API-side validation). The delivery
	// path fails safe: '' and unknown values resolve to full.
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
			// SCHED-GAP-1602: every sub-action is a mutation — identity
			// required; target names the object for the audit record.
			if !s.requireOperator(w, r, "project "+name+" pause") {
				return
			}
			s.pauseProject(w, r, name)
			return
		case "resume":
			if !s.requireOperator(w, r, "project "+name+" resume") {
				return
			}
			s.resumeProject(w, r, name)
			return
		case "spawn":
			if !s.requireOperator(w, r, "project "+name+" spawn") {
				return
			}
			s.spawnProject(w, r, name)
			return
		case "bump":
			if !s.requireOperator(w, r, "project "+name+" bump") {
				return
			}
			s.bumpProject(w, r, name)
			return
		case "unbump":
			if !s.requireOperator(w, r, "project "+name+" unbump") {
				return
			}
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
		// SCHED-GAP-1602: update is a mutation — identity required.
		if !s.requireOperator(w, r, "project "+name) {
			return
		}
		s.updateProject(w, r, name)
	case http.MethodDelete:
		// SCHED-GAP-1602: delete is a mutation — identity required.
		if !s.requireOperator(w, r, "project "+name) {
			return
		}
		s.deleteProject(w, r, name)
	default:
		writeError(w, 405, "GET, PUT, POST, or DELETE only")
	}
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, name string) {
	// SCHED-GAP-1575-B2: detail handler for /api/v1/projects/{name} shares the
	// single serialized SQLite connection (SetMaxOpenConns(1)) with the list
	// surface, so a stalled database.GetProject / getLatestTick would hang the
	// dashboard detail page the same way /status hung. Mirror the parent
	// pattern: deadline from r.Context() (never a bare context.Background()),
	// obs.enter / obs.check around each DB call, 504 on stall naming the step.
	ctx, obs := s.newRequestDeadline(r.Context(), "project", s.readTimeout())
	defer obs.finish()
	obs.enter("GetProject")
	p, err := database.GetProject(ctx, s.db, name)
	if !obs.check(w, ctx) {
		return
	}
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		writeError(w, 500, err.Error())
		return
	}
	obs.enter("getLatestTick")
	tick, _ := getLatestTick(ctx, s.db, name)
	if !obs.check(w, ctx) {
		return
	}
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
	// SCHED-GAP-150: two ways a retired driver can reach an ENABLED project
	// through PUT — (a) the update itself installs the command, (b) the update
	// re-enables a row whose stored command already drives one (a disabled
	// legacy lane). Both are refused here, before the DB write; the operator
	// must clear the command (or pick a supported executor) first.
	if updates.Command != nil && isRetiredCommand(*updates.Command) {
		writeError(w, 400, retiredCommandError(*updates.Command))
		return
	}
	if updates.Enabled != nil && *updates.Enabled && !wasEnabled && isRetiredCommand(cur.Command) {
		writeError(w, 400, retiredCommandError(cur.Command))
		return
	}
	if err := database.UpdateProject(ctx, s.db, name, updates); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, 404, "project not found")
			return
		}
		// SCHED-GAP-141: invalid board_ownership surfaces as 400.
		if strings.Contains(err.Error(), "invalid board_ownership") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// SCHED-GAP-124: invalid admission_mode surfaces as 400.
		if strings.Contains(err.Error(), "invalid admission_mode") {
			writeError(w, 400, err.Error())
			return
		}
		// SCHED-GAP-1586: a parent reference that would make the lane its
		// own ancestor (self-parent or longer loop) is a client-correctable
		// 400, not a server fault.
		if strings.Contains(err.Error(), "cycle") {
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
		// SCHED-GAP-180: the PUT surface is a second entry point to the
		// same pause operation — cascade to satellite lanes identically
		// (logged, never poisons the response).
		if err := cascadePauseToSatellites(ctx, s.db, name, "PUT /projects/{name}"); err != nil {
			log.Printf("SCHED-GAP-180: PUT disable %s: satellite cascade: %v", name, err)
		}
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
	// SCHED-GAP-180: the pause cascades to satellite lanes (<name>-qa/-pm/
	// -sync/-dogfood). A paused primary whose board still carries pending
	// rows would otherwise leave its satellites admitting ticks through the
	// shared board (heading stayed enabled 7 weeks this way). Cascade
	// failures are logged, never poison the response — the primary's pause
	// has already landed, and the invariant check is the backstop.
	if err := cascadePauseToSatellites(ctx, s.db, name, by); err != nil {
		log.Printf("SCHED-GAP-180: pause %s: satellite cascade: %v", name, err)
	}
	// SCHED-GAP-219: the fleet.toml regen is RETIRED. The DB is the cooldown
	// authority and the seed-only loader no longer re-pins `enabled` (or
	// cooldown/model/provider) from fleet.toml, so a pause that lives only
	// in the DB SURVIVES a restart — there is nothing left to resync. The
	// policy script that this call used to shell out to is itself retired
	// as a writer (SCHED-GAP-025's two-config law is closed).
	writeJSON(w, 200, map[string]string{"status": "paused", "project": name})
}

func (s *Server) resumeProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	if err := database.UpdateProject(ctx, s.db, name, database.ProjectUpdates{Enabled: database.BoolPtr(true)}); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	// SCHED-GAP-180: mirror the pause cascade — restore only the satellites
	// THIS resume owns (cascade provenance naming this target). A satellite
	// disabled for its own reason keeps its provenance and stays disabled.
	// Failures are logged, never poison the response (mirror of pause).
	if err := cascadeResumeSatellites(ctx, s.db, name); err != nil {
		log.Printf("SCHED-GAP-180: resume %s: satellite cascade: %v", name, err)
	}
	// SCHED-GAP-219: the fleet.toml regen is RETIRED (mirror of pause — the
	// DB is the authority and the seed-only loader never re-pins enabled).
	writeJSON(w, 200, map[string]string{"status": "resumed", "project": name})
}

// satelliteLaneSuffixes are the satellite lane name suffixes the pause/resume
// cascade recognizes (SCHED-GAP-180). Name suffix is the sanctioned detection
// for this row; board-ownership/symlink detection is SCHED-GAP-141's concern
// and is deliberately NOT walked here.
var satelliteLaneSuffixes = []string{"-qa", "-pm", "-sync", "-dogfood"}

// cascadeMarker is the disabled_by value stamped on satellites disabled by
// the pause/resume cascade (distinct from "api-pause" / "api" / "api-delete"
// / "auto-disable" so resume can restore exactly the rows the cascade owns).
const cascadeMarker = "api-pause-cascade"

// cascadeReasonPrefix is the disabled_reason prefix stamped on cascade-paused
// satellites; the full reason is "<prefix><primary> (via <provenance>)".
const cascadeReasonPrefix = "paused by target "

// cascadePauseToSatellites disables the satellite lanes of the named primary
// that are CURRENTLY enabled. Each newly disabled satellite is stamped
// disabled_by=cascadeMarker and disabled_reason="<cascadeReasonPrefix><primary>
// (via <provenance>)" so the matching resume can identify exactly the rows it
// owns. Satellites that do not exist are skipped; satellites already disabled
// keep their own provenance untouched. An error is returned only when the DB
// update itself fails; the caller logs it and proceeds (the primary's pause
// has already landed).
func cascadePauseToSatellites(ctx context.Context, db *sql.DB, name, provenance string) error {
	reason := cascadeReasonPrefix + name + " (via " + provenance + ")"
	for _, suffix := range satelliteLaneSuffixes {
		sat := name + suffix
		p, err := database.GetProject(ctx, db, sat)
		if err != nil {
			if errors.Is(err, database.ErrProjectNotFound) {
				continue
			}
			return fmt.Errorf("lookup satellite %q: %w", sat, err)
		}
		if !p.Enabled {
			continue
		}
		by := cascadeMarker
		if err := database.UpdateProject(ctx, db, sat, database.ProjectUpdates{
			Enabled:        database.BoolPtr(false),
			DisabledBy:     &by,
			DisabledReason: &reason,
		}); err != nil {
			return fmt.Errorf("cascade-pause satellite %q: %w", sat, err)
		}
		// GAP-044 parity: log the event from the row's actually-stamped
		// values (the DB layer owns disabled_at), mirroring the primary.
		if sp, err := database.GetProject(ctx, db, sat); err == nil {
			logDisableEvent(ctx, db, sat, sp.DisabledBy, sp.DisabledReason, sp.DisabledAt)
		}
	}
	return nil
}

// cascadeResumeSatellites re-enables the satellite lanes of the named primary
// that the cascade itself disabled for THIS target: disabled_by=cascadeMarker
// AND disabled_reason prefix "<cascadeReasonPrefix><name> ". A satellite
// paused for its own reason (disabled_by "api", "api-pause", "auto-disable",
// "api-delete", or a cascade naming a DIFFERENT target) is never touched.
// Missing satellites are skipped. An error is returned only when the DB
// update itself fails; the caller logs it and proceeds.
func cascadeResumeSatellites(ctx context.Context, db *sql.DB, name string) error {
	prefix := cascadeReasonPrefix + name + " "
	for _, suffix := range satelliteLaneSuffixes {
		sat := name + suffix
		p, err := database.GetProject(ctx, db, sat)
		if err != nil {
			if errors.Is(err, database.ErrProjectNotFound) {
				continue
			}
			return fmt.Errorf("lookup satellite %q: %w", sat, err)
		}
		if p.Enabled || p.DisabledBy != cascadeMarker || !strings.HasPrefix(p.DisabledReason, prefix) {
			continue
		}
		if err := database.UpdateProject(ctx, db, sat, database.ProjectUpdates{Enabled: database.BoolPtr(true)}); err != nil {
			return fmt.Errorf("cascade-resume satellite %q: %w", sat, err)
		}
		details, _ := json.Marshal(map[string]string{"project": sat, "target": name})
		_ = database.LogEvent(ctx, db, &database.Event{
			Severity:  database.SeverityInfo,
			Component: "api",
			Message:   fmt.Sprintf("project enabled: %s (cascade resume of %s)", sat, name),
			Details:   string(details),
		})
	}
	return nil
}

// regenFleetTomlViaPolicy RETIRED (SCHED-GAP-219): pause/resume no longer
// regenerate fleet.toml. The DB is the cooldown authority and the seed-only
// loader (internal/config ApplyFleetConfig) never re-pins enabled/cooldown/
// model/provider for an existing row, so an API state change is durable
// across restarts without a toml mirror. The function body is gone; the
// policyScriptPath/regenFleetTomlExec helpers below remain ONLY as the
// documented seam for the drift probe in server_config.go (a read-only
// check, never a writer).

// policyScriptPath resolves the ops policy script from the RUNNING user's home
// rather than a hardcoded /home/kara. RETIRED as a writer (SCHED-GAP-219);
// kept only for the SCHEDULER_POLICY_SCRIPT test override contract.
func policyScriptPath() string {
	const rel = "scripts/fleet-cooldown-policy.py"
	if p := os.Getenv("SCHEDULER_POLICY_SCRIPT"); p != "" {
		return p
	}
	if hh := os.Getenv("HERMES_HOME"); hh != "" {
		return filepath.Join(hh, rel)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".hermes", rel)
	}
	return "/home/kara/.hermes/" + rel
}

var regenFleetTomlExec = func() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", policyScriptPath(), "--apply")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Non-zero exit is logged via the returned error, never an API
		// failure — the policy script owns its success criterion.
		return fmt.Errorf("policy script failed (stderr: %s): %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

// regenFleetTomlViaPolicy RETIRED (SCHED-GAP-219): removed with the pause/
// resume call sites. regenFleetTomlExec stays as an injectable no-op only
// because the ops-script seam remains referenced by the policy-script test
// override contract; nothing in production invokes it.

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
//
// SCHED-GAP-1620: a DISABLED project is refused with 409 before SpawnNow,
// mirroring bumpProject — a lane an operator paused must not fire because a
// script re-ran a manual spawn.
func (s *Server) spawnProject(w http.ResponseWriter, r *http.Request, name string) {
	ctx := context.Background()
	p, err := database.GetProject(ctx, s.db, name)
	if err != nil {
		writeError(w, 404, "project not found")
		return
	}
	if !p.Enabled {
		writeError(w, 409, "project is disabled — resume it before spawning")
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
