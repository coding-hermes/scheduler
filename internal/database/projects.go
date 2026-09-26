package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coding-hermes/scheduler/internal/clock"
)

// ErrProjectNotFound is returned when a project lookup or update targets a
// name that does not exist in the projects table.
var ErrProjectNotFound = errors.New("project not found")

// nowUTC returns the current time as a UTC RFC3339 string — the canonical
// timestamp format stored in all TEXT timestamp columns.
//
// The clock comes from the context (SCHED-GAP-169): database helpers own no
// component to hang a Clock on, and every one of them already threads a ctx, so
// clock.WithClock(ctx, c) is the injection point. Without it the wall clock is
// used, exactly as before the seam.
func nowUTC(ctx context.Context) string {
	return clock.FromContext(ctx).Now().UTC().Format(time.RFC3339)
}

// CreateProject inserts a new project row. CreatedAt and UpdatedAt are set
// to the current UTC time if the caller left them zero-valued.
func CreateProject(ctx context.Context, db *sql.DB, p *Project) error {
	if p.CreatedAt == "" {
		p.CreatedAt = nowUTC(ctx)
	}
	if p.UpdatedAt == "" {
		p.UpdatedAt = p.CreatedAt
	}
	// Case-insensitive workdir uniqueness — prevents ghost duplicate projects
	// (e.g. "heading" vs "HEADING" pointing at the same directory). The daemon
	// and scheduler treat project names as case-sensitive, so two entries can
	// share a workdir and split ticks unpredictably. Refuse at creation when
	// the existing project is ENABLED (two active foremen, same board). A
	// disabled duplicate is harmless (archived entry).
	if p.Workdir != "" {
		var existing string
		var existingEnabled int
		err := db.QueryRowContext(ctx,
			`SELECT name, enabled FROM projects WHERE LOWER(workdir) = LOWER(?) LIMIT 1`,
			p.Workdir).Scan(&existing, &existingEnabled)
		if err == nil && existingEnabled == 1 {
			return fmt.Errorf("create project %q: workdir %q already registered by enabled project %q (case-insensitive duplicate)",
				p.Name, p.Workdir, existing)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("create project %q: duplicate workdir check: %w", p.Name, err)
		}
	}
	// Case-insensitive name uniqueness — prevents ghost duplicate projects
	// (e.g. "heading" vs "HEADING") that SQLite's case-sensitive TEXT PRIMARY
	// KEY would otherwise allow. Refuse at creation when the existing project
	// is ENABLED; a disabled duplicate is harmless (archived entry). Mirrors
	// the workdir check above.
	if p.Name != "" {
		var existingName string
		var existingNameEnabled int
		err := db.QueryRowContext(ctx,
			`SELECT name, enabled FROM projects WHERE LOWER(name) = LOWER(?) LIMIT 1`,
			p.Name).Scan(&existingName, &existingNameEnabled)
		if err == nil && existingNameEnabled == 1 {
			return fmt.Errorf("create project %q: name %q already registered by enabled project %q (case-insensitive duplicate)",
				p.Name, p.Name, existingName)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("create project %q: duplicate name check: %w", p.Name, err)
		}
	}
	const q = `INSERT INTO projects
(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, deliver_mode, parent, enabled, created_at, updated_at, adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s, no_progress_threshold, no_progress_ticks, board_rows_seen, admission_mode, board_ownership, target_runs_per_day)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	// A zero-valued BoardRowsSeen on a brand-new row would read as "board
	// observed with 0 rows"; store the unseen sentinel instead so the first
	// adaptive observation only ever establishes a baseline.
	boardRowsSeen := p.BoardRowsSeen
	if boardRowsSeen == 0 {
		boardRowsSeen = AdaptiveUnseenBoardRows
	}
	// SCHED-GAP-141 (durability): admission_mode and board_ownership are
	// part of the INSERT. Both are blocking-gate knobs, and the create path
	// is what fleet.toml uses for a project it has not seen before — before
	// this they were dropped at creation and only re-applied on the NEXT
	// restart's pin pass, so a brand-new lane ran one full load on the
	// inherited defaults while the file said otherwise.
	_, err := db.ExecContext(ctx, q,
		p.Name, p.RepoURL, p.Workdir, p.Weight, p.Priority, p.CooldownS,
		p.DecayRate, p.Model, p.Provider, p.FallbackModel, p.FallbackProvider, boolToInt(p.NoGlobalFallback),
		p.IdleModel, p.IdleProvider,
		p.DailyBudgetUSD, p.WeeklyBudgetUSD, p.FinalBudgetUSD,
		p.WorkerModel, p.WorkerProvider, p.GatewayKey, p.Command, p.Prompt, p.PromptMode, p.NamespaceID, p.Deliver, p.DeliverMode, p.Parent, boolToInt(p.Enabled),
		p.CreatedAt, p.UpdatedAt,
		boolToInt(p.AdaptiveCooldown), p.CooldownFloorS, p.CooldownCeilingS, p.NoProgressThreshold, p.NoProgressTicks, boardRowsSeen, p.AdmissionMode, p.BoardOwnership,
		p.TargetRunsPerDay)
	if err != nil {
		return fmt.Errorf("create project %q: %w", p.Name, err)
	}
	return nil
}

// SCHED-GAP-124 admission modes. AdmissionModeCooldown is the classic
// wall-clock cron gate; AdmissionModeTasks admits a project whenever its
// board holds non-perpetual pending work (perpetual fixtures excluded via
// boardRowIsFixture — GAP-106), falling back to the cooldown pin when
// drained. Shared by namespaces (default) and projects (override).
const (
	AdmissionModeCooldown = "cooldown"
	AdmissionModeTasks    = "tasks"
)

// SCHED-GAP-141 board ownership. BoardOwnershipAuto is the default for every
// existing row: ownership is DERIVED from the board walk (after symlink
// resolution the board a lane reads must live inside that lane's own
// workdir), so the satellite-lane shape — a board linked in from another
// project's workdir — is paced by its cooldown instead of waiving it. The
// explicit values exist so unusual layouts are configurable rather than
// special-cased in code: "owner" asserts ownership the path check cannot
// see, "shared" declares a foreign board the path check would miss (a copy
// or bind-mounted board). Validated at write time by UpdateProject and
// pinned from fleet.toml by the loader.
const (
	BoardOwnershipAuto   = ""
	BoardOwnershipOwner  = "owner"
	BoardOwnershipShared = "shared"
)

// GetProject loads a single project by name. Returns ErrProjectNotFound if
// no row matches.
func GetProject(ctx context.Context, db *sql.DB, name string) (*Project, error) {
	const q = `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, model_chain, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, deliver_mode, enabled, created_at, updated_at, consecutive_failures, COALESCE(last_tick_started, ''), COALESCE(last_tick_completed, ''), COALESCE(disabled_at, ''), COALESCE(disabled_by, ''), COALESCE(disabled_reason, ''), COALESCE(adaptive_cooldown, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0), COALESCE(no_progress_threshold, 0), COALESCE(no_progress_ticks, 0), COALESCE(board_rows_seen, -1), COALESCE(bump_active, 0), COALESCE(bump_remaining_ticks, 0), COALESCE(bump_cooldown_s, 0), COALESCE(bump_reason, ''), COALESCE(bump_saved_cooldown_s, 0), COALESCE(bump_saved_floor_s, 0), COALESCE(bump_saved_ceiling_s, 0), COALESCE(bump_saved_no_progress_ticks, 0), COALESCE(bump_started_at, ''), COALESCE(admission_mode, ''), COALESCE(board_ownership, ''), cooldown_pin_s, COALESCE(cooldown_pin_by, ''), COALESCE(cooldown_pin_at, ''), COALESCE(last_tick_status, ''), COALESCE(parent, ''), target_runs_per_day
FROM projects WHERE name = ?`
	var p Project
	var enabled int
	var nsID sql.NullString
	var pinS sql.NullInt64
	var targetRunsPerDay sql.NullFloat64
	err := db.QueryRowContext(ctx, q, name).Scan(
		&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
		&p.DecayRate, &p.Model, &p.Provider, &p.FallbackModel, &p.FallbackProvider, &p.NoGlobalFallback, &p.ModelChain, &p.IdleModel, &p.IdleProvider,
		&p.DailyBudgetUSD, &p.WeeklyBudgetUSD, &p.FinalBudgetUSD,
		&p.WorkerModel, &p.WorkerProvider, &p.GatewayKey, &p.Command, &p.Prompt, &p.PromptMode, &nsID, &p.Deliver, &p.DeliverMode, &enabled, &p.CreatedAt, &p.UpdatedAt, &p.ConsecutiveFailures, &p.LastTickStarted, &p.LastTickCompleted, &p.DisabledAt, &p.DisabledBy, &p.DisabledReason,
		&p.AdaptiveCooldown, &p.CooldownFloorS, &p.CooldownCeilingS, &p.NoProgressThreshold, &p.NoProgressTicks, &p.BoardRowsSeen,
		&p.BumpActive, &p.BumpRemainingTicks, &p.BumpCooldownS, &p.BumpReason, &p.BumpSavedCooldownS, &p.BumpSavedFloorS, &p.BumpSavedCeilingS, &p.BumpSavedNoProgress, &p.BumpStartedAt, &p.AdmissionMode, &p.BoardOwnership,
		&pinS, &p.CooldownPinBy, &p.CooldownPinAt, &p.LastTickStatus, &p.Parent, &targetRunsPerDay)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, name)
	}
	if err != nil {
		return nil, fmt.Errorf("get project %q: %w", name, err)
	}
	p.Enabled = enabled != 0
	if nsID.Valid {
		p.NamespaceID = &nsID.String
	}
	if pinS.Valid {
		v := int(pinS.Int64)
		p.CooldownPinS = &v
	}
	if targetRunsPerDay.Valid {
		v := targetRunsPerDay.Float64
		p.TargetRunsPerDay = &v
	}
	return &p, nil
}

// ListProjects returns projects. If enabledOnly is true, only enabled=1
// rows are returned. Results are ordered by name for stable output.
func ListProjects(ctx context.Context, db *sql.DB, enabledOnly bool) ([]Project, error) {
	q := `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, model_chain, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, deliver_mode, enabled, created_at, updated_at, consecutive_failures, COALESCE(last_tick_started, ''), COALESCE(last_tick_completed, ''), COALESCE(disabled_at, ''), COALESCE(disabled_by, ''), COALESCE(disabled_reason, ''), COALESCE(adaptive_cooldown, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0), COALESCE(no_progress_threshold, 0), COALESCE(no_progress_ticks, 0), COALESCE(board_rows_seen, -1), COALESCE(bump_active, 0), COALESCE(bump_remaining_ticks, 0), COALESCE(bump_cooldown_s, 0), COALESCE(bump_reason, ''), COALESCE(bump_saved_cooldown_s, 0), COALESCE(bump_saved_floor_s, 0), COALESCE(bump_saved_ceiling_s, 0), COALESCE(bump_saved_no_progress_ticks, 0), COALESCE(bump_started_at, ''), COALESCE(admission_mode, ''), COALESCE(board_ownership, ''), cooldown_pin_s, COALESCE(cooldown_pin_by, ''), COALESCE(cooldown_pin_at, ''), COALESCE(last_tick_status, ''), COALESCE(parent, ''), target_runs_per_day
FROM projects`
	if enabledOnly {
		q += " WHERE enabled = 1"
	}
	q += " ORDER BY name ASC"

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		var enabled int
		var nsID sql.NullString
		var pinS sql.NullInt64
		var targetRunsPerDay sql.NullFloat64
		if err := rows.Scan(
			&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
			&p.DecayRate, &p.Model, &p.Provider, &p.FallbackModel, &p.FallbackProvider, &p.NoGlobalFallback, &p.ModelChain, &p.IdleModel, &p.IdleProvider,
			&p.DailyBudgetUSD, &p.WeeklyBudgetUSD, &p.FinalBudgetUSD,
			&p.WorkerModel, &p.WorkerProvider, &p.GatewayKey, &p.Command, &p.Prompt, &p.PromptMode, &nsID, &p.Deliver, &p.DeliverMode, &enabled,
			&p.CreatedAt, &p.UpdatedAt, &p.ConsecutiveFailures, &p.LastTickStarted, &p.LastTickCompleted, &p.DisabledAt, &p.DisabledBy, &p.DisabledReason,
			&p.AdaptiveCooldown, &p.CooldownFloorS, &p.CooldownCeilingS, &p.NoProgressThreshold, &p.NoProgressTicks, &p.BoardRowsSeen,
			&p.BumpActive, &p.BumpRemainingTicks, &p.BumpCooldownS, &p.BumpReason, &p.BumpSavedCooldownS, &p.BumpSavedFloorS, &p.BumpSavedCeilingS, &p.BumpSavedNoProgress, &p.BumpStartedAt, &p.AdmissionMode, &p.BoardOwnership,
			&pinS, &p.CooldownPinBy, &p.CooldownPinAt, &p.LastTickStatus, &p.Parent, &targetRunsPerDay); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.Enabled = enabled != 0
		if nsID.Valid {
			p.NamespaceID = &nsID.String
		}
		if pinS.Valid {
			v := int(pinS.Int64)
			p.CooldownPinS = &v
		}
		if targetRunsPerDay.Valid {
			v := targetRunsPerDay.Float64
			p.TargetRunsPerDay = &v
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}
	return out, nil
}

// ListProjectsByNamespace returns all projects assigned to the given namespace,
// ordered by name. Returns an empty slice if no projects match.
func ListProjectsByNamespace(ctx context.Context, db *sql.DB, namespaceID string) ([]Project, error) {
	q := `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, model_chain, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, deliver_mode, enabled, created_at, updated_at, consecutive_failures, COALESCE(last_tick_started, ''), COALESCE(last_tick_completed, ''), COALESCE(disabled_at, ''), COALESCE(disabled_by, ''), COALESCE(disabled_reason, ''), COALESCE(adaptive_cooldown, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0), COALESCE(no_progress_threshold, 0), COALESCE(no_progress_ticks, 0), COALESCE(board_rows_seen, -1), COALESCE(bump_active, 0), COALESCE(bump_remaining_ticks, 0), COALESCE(bump_cooldown_s, 0), COALESCE(bump_reason, ''), COALESCE(bump_saved_cooldown_s, 0), COALESCE(bump_saved_floor_s, 0), COALESCE(bump_saved_ceiling_s, 0), COALESCE(bump_saved_no_progress_ticks, 0), COALESCE(bump_started_at, ''), COALESCE(admission_mode, ''), COALESCE(board_ownership, ''), cooldown_pin_s, COALESCE(cooldown_pin_by, ''), COALESCE(cooldown_pin_at, ''), COALESCE(last_tick_status, ''), COALESCE(parent, ''), target_runs_per_day
FROM projects WHERE namespace_id = ? ORDER BY name ASC`

	rows, err := db.QueryContext(ctx, q, namespaceID)
	if err != nil {
		return nil, fmt.Errorf("list projects by namespace %q: %w", namespaceID, err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		var enabled int
		var nsID sql.NullString
		var pinS sql.NullInt64
		var targetRunsPerDay sql.NullFloat64
		if err := rows.Scan(
			&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
			&p.DecayRate, &p.Model, &p.Provider, &p.FallbackModel, &p.FallbackProvider, &p.NoGlobalFallback, &p.ModelChain, &p.IdleModel, &p.IdleProvider,
			&p.DailyBudgetUSD, &p.WeeklyBudgetUSD, &p.FinalBudgetUSD,
			&p.WorkerModel, &p.WorkerProvider, &p.GatewayKey, &p.Command, &p.Prompt, &p.PromptMode, &nsID, &p.Deliver, &p.DeliverMode, &enabled,
			&p.CreatedAt, &p.UpdatedAt, &p.ConsecutiveFailures, &p.LastTickStarted, &p.LastTickCompleted, &p.DisabledAt, &p.DisabledBy, &p.DisabledReason,
			&p.AdaptiveCooldown, &p.CooldownFloorS, &p.CooldownCeilingS, &p.NoProgressThreshold, &p.NoProgressTicks, &p.BoardRowsSeen,
			&p.BumpActive, &p.BumpRemainingTicks, &p.BumpCooldownS, &p.BumpReason, &p.BumpSavedCooldownS, &p.BumpSavedFloorS, &p.BumpSavedCeilingS, &p.BumpSavedNoProgress, &p.BumpStartedAt, &p.AdmissionMode, &p.BoardOwnership,
			&pinS, &p.CooldownPinBy, &p.CooldownPinAt, &p.LastTickStatus, &p.Parent, &targetRunsPerDay); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.Enabled = enabled != 0
		if nsID.Valid {
			p.NamespaceID = &nsID.String
		}
		if pinS.Valid {
			v := int(pinS.Int64)
			p.CooldownPinS = &v
		}
		if targetRunsPerDay.Valid {
			v := targetRunsPerDay.Float64
			p.TargetRunsPerDay = &v
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}
	if out == nil {
		out = []Project{}
	}
	return out, nil
}

// ProjectUpdates holds the optional fields for a partial project update.
// Only non-nil fields are written. Pointer types distinguish "unset" from
// "set to zero value".
type ProjectUpdates struct {
	RepoURL          *string  `json:"repo_url"`
	Workdir          *string  `json:"workdir"`
	Weight           *int     `json:"weight"`
	Priority         *int     `json:"priority"`
	CooldownS        *int     `json:"cooldown_s"`
	TargetRunsPerDay *float64 `json:"target_runs_per_day"` // nil = unchanged/derive, 0 = no opinion, >0 = explicit target
	DecayRate        *float64 `json:"decay_rate"`
	Model            *string  `json:"model"`
	Provider         *string  `json:"provider"`
	FallbackModel    *string  `json:"fallback_model"` // SCHED-GAP-064: project fallback tier for the spawn chain
	FallbackProvider *string  `json:"fallback_provider"`
	NoGlobalFallback *bool    `json:"no_global_fallback"` // true → skip the spawner-level (env) fallback tier
	IdleModel        *string  `json:"idle_model"`         // SCHED-GAP-065: project idle tier, prepended to the spawn chain on zero-pending boards
	IdleProvider     *string  `json:"idle_provider"`      // SCHED-GAP-065: idle provider tier; "" clears back to no project idle lane
	DailyBudgetUSD   *float64 `json:"daily_budget_usd"`   // SCHED-GAP-066: per-UTC-day spend cap; <= 0 = unlimited
	WeeklyBudgetUSD  *float64 `json:"weekly_budget_usd"`  // per-UTC-week spend cap (Monday 00:00 UTC reset); <= 0 = unlimited
	FinalBudgetUSD   *float64 `json:"final_budget_usd"`   // one-time lifetime spend cap, never resets; <= 0 = unlimited
	WorkerModel      *string  `json:"worker_model"`
	WorkerProvider   *string  `json:"worker_provider"`
	GatewayKey       *string  `json:"gateway_key"` // per-foreman Hermes gateway key; "" clears back to shared key
	Command          *string  `json:"command"`
	Prompt           *string  `json:"prompt"`       // Bane 2026-08-27: extra foreman prompt; "" clears back to namespace default only
	PromptMode       *string  `json:"prompt_mode"`  // Bane 2026-08-27: "append" (default) | "replace"
	NamespaceID      *string  `json:"namespace_id"` // set to "" to unassign from namespace
	ModelChain       *string  `json:"model_chain"`  // SCHED-GAP-095: ordered "model@provider" hops (JSON array); "" clears
	Deliver          *string  `json:"deliver"`      // SCHED-GAP-095: delivery target platform:chat_id:thread_id; "" clears
	DeliverMode      *string  `json:"deliver_mode"` // SCHED-GAP-1607: "full" (default) | "file" | "link"; "" resets to full
	// SCHED-GAP-1586: parent lane reference — the name of the lane this lane
	// is a satellite of; "" clears back to a primary/root lane. Every write
	// is cycle-checked (self-parenting and any chain that loops back to the
	// lane are refused); a parent that names a non-existent lane is
	// tolerated at write time (dangling, resolved as a root child).
	Parent  *string `json:"parent"`
	Enabled *bool   `json:"enabled"`
	// Disable provenance overrides (GAP-044): when Enabled transitions
	// true→false, DisabledBy/DisabledReason default to "api"/"disabled via
	// API update" unless explicitly supplied here; DisabledAt defaults to
	// the update time. A false→true transition clears all three.
	DisabledAt     *string `json:"disabled_at"`
	DisabledBy     *string `json:"disabled_by"`
	DisabledReason *string `json:"disabled_reason"`

	// Adaptive-cooldown policy fields. Enabling adaptive_cooldown (false→true)
	// normalizes the policy row: cooldown_floor_s defaults to the current
	// cooldown_s, cooldown_ceiling_s to DefaultAdaptiveCooldownCeilingS and
	// no_progress_threshold to DefaultAdaptiveCooldownThreshold when not
	// explicitly supplied, and the runtime streak (no_progress_ticks /
	// board_rows_seen) is reset to a clean slate. Setting adaptive_cooldown
	// to false leaves the stored policy values untouched (harmless while off).
	AdaptiveCooldown    *bool `json:"adaptive_cooldown"`
	CooldownFloorS      *int  `json:"cooldown_floor_s"`
	CooldownCeilingS    *int  `json:"cooldown_ceiling_s"`
	NoProgressThreshold *int  `json:"no_progress_threshold"`

	// SCHED-GAP-124: per-project admission-mode override; "" = inherit
	// namespace, "cooldown" or "tasks" otherwise (validated).
	AdmissionMode *string `json:"admission_mode"`

	// SCHED-GAP-141: per-project board-ownership override; "" = auto
	// (derived from the board walk), "owner" or "shared" otherwise
	// (validated).
	BoardOwnership *string `json:"board_ownership"`

	// SCHED-GAP-219 cooldown pin. CooldownPinS semantics:
	//   nil            — leave the pin untouched
	//   pointer to >0  — set/raise the pin (never silently LOWERED: a
	//                    value below the existing pin is a 400 at the API
	//                    layer and a no-op with a log line at the loader).
	//                    Setting the pin also snaps cooldown_s UP to the
	//                    pin when the live cooldown sits below it, so the
	//                    pin takes effect immediately without waiting for
	//                    the next policy pass.
	//                    An EQUAL value re-stamps provenance (idempotent).
	// ClearCooldownPin — true removes the pin (and its provenance).
	CooldownPinS     *int  `json:"cooldown_pin_s"`
	ClearCooldownPin *bool `json:"clear_cooldown_pin"`
}

// UnmarshalJSON decodes ProjectUpdates from JSON. Canonical keys are
// snake_case, but live fleet automation (fleet-auto-heal, stand-in
// gap-pusher scripts) still PUTs the legacy PascalCase Go field names
// (CooldownS, Enabled, DecayRate, …). After the standard tag-based decode,
// each nil pointer is back-filled from its legacy PascalCase key so both
// spellings keep working. Explicit zero values (e.g. "enabled": false)
// always bind through the pointer and are never overridden.
func (u *ProjectUpdates) UnmarshalJSON(data []byte) error {
	// Alias avoids infinite recursion through this method.
	type updatesAlias ProjectUpdates
	var a updatesAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*u = ProjectUpdates(a)

	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	fill := func(key string, dst any, set func()) {
		raw, ok := legacy[key]
		if !ok {
			return
		}
		if err := json.Unmarshal(raw, dst); err == nil {
			set()
		}
	}
	if u.RepoURL == nil {
		var v string
		fill("RepoURL", &v, func() { u.RepoURL = &v })
	}
	if u.Workdir == nil {
		var v string
		fill("Workdir", &v, func() { u.Workdir = &v })
	}
	if u.Weight == nil {
		var v int
		fill("Weight", &v, func() { u.Weight = &v })
	}
	if u.Priority == nil {
		var v int
		fill("Priority", &v, func() { u.Priority = &v })
	}
	if u.CooldownS == nil {
		var v int
		fill("CooldownS", &v, func() { u.CooldownS = &v })
	}
	if u.TargetRunsPerDay == nil {
		var v float64
		fill("TargetRunsPerDay", &v, func() { u.TargetRunsPerDay = &v })
	}
	if u.DecayRate == nil {
		var v float64
		fill("DecayRate", &v, func() { u.DecayRate = &v })
	}
	if u.Model == nil {
		var v string
		fill("Model", &v, func() { u.Model = &v })
	}
	if u.Provider == nil {
		var v string
		fill("Provider", &v, func() { u.Provider = &v })
	}
	if u.FallbackModel == nil {
		var v string
		fill("FallbackModel", &v, func() { u.FallbackModel = &v })
	}
	if u.FallbackProvider == nil {
		var v string
		fill("FallbackProvider", &v, func() { u.FallbackProvider = &v })
	}
	if u.IdleModel == nil {
		var v string
		fill("IdleModel", &v, func() { u.IdleModel = &v })
	}
	if u.IdleProvider == nil {
		var v string
		fill("IdleProvider", &v, func() { u.IdleProvider = &v })
	}
	if u.NoGlobalFallback == nil {
		var v bool
		fill("NoGlobalFallback", &v, func() { u.NoGlobalFallback = &v })
	}
	if u.WorkerModel == nil {
		var v string
		fill("WorkerModel", &v, func() { u.WorkerModel = &v })
	}
	if u.WorkerProvider == nil {
		var v string
		fill("WorkerProvider", &v, func() { u.WorkerProvider = &v })
	}
	if u.GatewayKey == nil {
		var v string
		fill("GatewayKey", &v, func() { u.GatewayKey = &v })
	}
	if u.Command == nil {
		var v string
		fill("Command", &v, func() { u.Command = &v })
	}
	if u.NamespaceID == nil {
		var v string
		fill("NamespaceID", &v, func() { u.NamespaceID = &v })
	}
	if u.ModelChain == nil {
		var v string
		fill("ModelChain", &v, func() { u.ModelChain = &v })
	}
	if u.Deliver == nil {
		var v string
		fill("Deliver", &v, func() { u.Deliver = &v })
	}
	if u.Parent == nil {
		var v string
		fill("Parent", &v, func() { u.Parent = &v })
	}
	if u.Enabled == nil {
		var v bool
		fill("Enabled", &v, func() { u.Enabled = &v })
	}
	if u.DisabledAt == nil {
		var v string
		fill("DisabledAt", &v, func() { u.DisabledAt = &v })
	}
	if u.DisabledBy == nil {
		var v string
		fill("DisabledBy", &v, func() { u.DisabledBy = &v })
	}
	if u.DisabledReason == nil {
		var v string
		fill("DisabledReason", &v, func() { u.DisabledReason = &v })
	}
	if u.AdaptiveCooldown == nil {
		var v bool
		fill("AdaptiveCooldown", &v, func() { u.AdaptiveCooldown = &v })
	}
	if u.CooldownFloorS == nil {
		var v int
		fill("CooldownFloorS", &v, func() { u.CooldownFloorS = &v })
	}
	if u.CooldownCeilingS == nil {
		var v int
		fill("CooldownCeilingS", &v, func() { u.CooldownCeilingS = &v })
	}
	if u.NoProgressThreshold == nil {
		var v int
		fill("NoProgressThreshold", &v, func() { u.NoProgressThreshold = &v })
	}
	if u.CooldownPinS == nil {
		var v int
		fill("CooldownPinS", &v, func() { u.CooldownPinS = &v })
	}
	if u.ClearCooldownPin == nil {
		var v bool
		fill("ClearCooldownPin", &v, func() { u.ClearCooldownPin = &v })
	}
	return nil
}

// UpdateProject applies the given updates to the project named name. Only
// the fields present in updates are modified; UpdatedAt is always refreshed.
//
// GAP-044 disable provenance: when Enabled transitions true→false the
// disabled_at/by/reason columns are stamped (defaults: now, "api",
// "disabled via API update" — callers may supply explicit overrides via
// the Disabled* fields). A false→true transition (resume) clears all
// three. Non-transition updates leave them untouched.
func UpdateProject(ctx context.Context, db *sql.DB, name string, updates ProjectUpdates) error {
	setClauses := []string{"updated_at = ?"}
	args := []any{nowUTC(ctx)}

	// GAP-044: resolve the enabled transition before building clauses.
	if updates.Enabled != nil {
		var curEnabled int
		err := db.QueryRowContext(ctx,
			`SELECT enabled FROM projects WHERE name = ?`, name).Scan(&curEnabled)
		if err != nil {
			return fmt.Errorf("read current enabled for %q: %w", name, err)
		}
		now := nowUTC(ctx)
		if curEnabled == 1 && !*updates.Enabled {
			// disable transition — stamp provenance (caller overrides win)
			if updates.DisabledAt == nil {
				updates.DisabledAt = &now
			}
			if updates.DisabledBy == nil {
				by := "api"
				updates.DisabledBy = &by
			}
			if updates.DisabledReason == nil {
				reason := "disabled via API update"
				updates.DisabledReason = &reason
			}
		} else if curEnabled == 0 && *updates.Enabled {
			// resume — clear provenance with explicit NULL clauses
			updates.DisabledAt = nil
			updates.DisabledBy = nil
			updates.DisabledReason = nil
			setClauses = append(setClauses,
				"disabled_at = NULL", "disabled_by = NULL", "disabled_reason = NULL")
		}
	}

	// Adaptive-cooldown enable transition (false→true): normalize the policy
	// row so the DB always holds EFFECTIVE values for an enabled project
	// (floor = current cooldown_s when not supplied, ceiling/threshold = the
	// built-in defaults) and clear the runtime streak for a clean slate.
	// The floor is snapshotted from cooldown_s at enable time so a later
	// reset has a durable base even after cooldown_s has been escalated.
	if updates.AdaptiveCooldown != nil && *updates.AdaptiveCooldown {
		var curCD, curAdaptive int
		err := db.QueryRowContext(ctx,
			`SELECT cooldown_s, adaptive_cooldown FROM projects WHERE name = ?`, name,
		).Scan(&curCD, &curAdaptive)
		if err != nil {
			return fmt.Errorf("read current adaptive state for %q: %w", name, err)
		}
		if curAdaptive == 0 {
			// Fresh enablement — reset the runtime streak and both board
			// baselines (SCHED-GAP-105: open-row baseline joins the reset).
			setClauses = append(setClauses,
				"no_progress_ticks = 0", "board_rows_seen = -1", "board_open_seen = -1")
			if updates.CooldownFloorS == nil && curCD > 0 {
				floor := curCD
				updates.CooldownFloorS = &floor
			}
			// Resolve the effective floor (explicit, else the snapshot
			// from cooldown_s above) so the derived ceiling (ADV-R10)
			// derives from THAT — 8 × floor, the fleet.toml pin shape.
			// Never a second hardcoded ceiling constant.
			floorForCeiling := 0
			if updates.CooldownFloorS != nil {
				floorForCeiling = *updates.CooldownFloorS
			}
			if updates.CooldownCeilingS == nil {
				ceiling := DefaultAdaptiveCooldownCeiling(floorForCeiling)
				if ceiling > 0 {
					updates.CooldownCeilingS = &ceiling
				}
			}
			if updates.NoProgressThreshold == nil {
				threshold := DefaultAdaptiveCooldownThreshold
				updates.NoProgressThreshold = &threshold
			}
		}
	}

	if updates.RepoURL != nil {
		setClauses = append(setClauses, "repo_url = ?")
		args = append(args, *updates.RepoURL)
	}
	if updates.Workdir != nil {
		setClauses = append(setClauses, "workdir = ?")
		args = append(args, *updates.Workdir)
	}
	if updates.Weight != nil {
		setClauses = append(setClauses, "weight = ?")
		args = append(args, *updates.Weight)
	}
	if updates.Priority != nil {
		setClauses = append(setClauses, "priority = ?")
		args = append(args, *updates.Priority)
	}
	if updates.CooldownS != nil {
		setClauses = append(setClauses, "cooldown_s = ?")
		args = append(args, *updates.CooldownS)
	}
	if updates.TargetRunsPerDay != nil {
		if *updates.TargetRunsPerDay < 0 {
			return fmt.Errorf("target_runs_per_day must be >= 0 for project %q", name)
		}
		setClauses = append(setClauses, "target_runs_per_day = ?")
		args = append(args, *updates.TargetRunsPerDay)
	}
	if updates.DecayRate != nil {
		setClauses = append(setClauses, "decay_rate = ?")
		args = append(args, *updates.DecayRate)
	}
	if updates.Model != nil {
		setClauses = append(setClauses, "model = ?")
		args = append(args, *updates.Model)
	}
	if updates.Provider != nil {
		setClauses = append(setClauses, "provider = ?")
		args = append(args, *updates.Provider)
	}
	if updates.FallbackModel != nil {
		setClauses = append(setClauses, "fallback_model = ?")
		args = append(args, *updates.FallbackModel)
	}
	if updates.FallbackProvider != nil {
		setClauses = append(setClauses, "fallback_provider = ?")
		args = append(args, *updates.FallbackProvider)
	}
	if updates.NoGlobalFallback != nil {
		setClauses = append(setClauses, "no_global_fallback = ?")
		args = append(args, boolToInt(*updates.NoGlobalFallback))
	}
	if updates.IdleModel != nil {
		setClauses = append(setClauses, "idle_model = ?")
		args = append(args, *updates.IdleModel)
	}
	if updates.IdleProvider != nil {
		setClauses = append(setClauses, "idle_provider = ?")
		args = append(args, *updates.IdleProvider)
	}
	if updates.DailyBudgetUSD != nil {
		setClauses = append(setClauses, "daily_budget_usd = ?")
		args = append(args, *updates.DailyBudgetUSD)
	}
	if updates.WeeklyBudgetUSD != nil {
		setClauses = append(setClauses, "weekly_budget_usd = ?")
		args = append(args, *updates.WeeklyBudgetUSD)
	}
	if updates.FinalBudgetUSD != nil {
		setClauses = append(setClauses, "final_budget_usd = ?")
		args = append(args, *updates.FinalBudgetUSD)
	}
	if updates.WorkerModel != nil {
		setClauses = append(setClauses, "worker_model = ?")
		args = append(args, *updates.WorkerModel)
	}
	if updates.WorkerProvider != nil {
		setClauses = append(setClauses, "worker_provider = ?")
		args = append(args, *updates.WorkerProvider)
	}
	if updates.GatewayKey != nil {
		setClauses = append(setClauses, "gateway_key = ?")
		args = append(args, *updates.GatewayKey)
	}
	if updates.Command != nil {
		setClauses = append(setClauses, "command = ?")
		args = append(args, *updates.Command)
	}
	if updates.Prompt != nil {
		setClauses = append(setClauses, "prompt = ?")
		args = append(args, *updates.Prompt)
	}
	if updates.PromptMode != nil {
		setClauses = append(setClauses, "prompt_mode = ?")
		args = append(args, *updates.PromptMode)
	}
	if updates.NamespaceID != nil {
		setClauses = append(setClauses, "namespace_id = ?")
		args = append(args, *updates.NamespaceID)
	}
	if updates.ModelChain != nil {
		setClauses = append(setClauses, "model_chain = ?")
		args = append(args, *updates.ModelChain)
	}
	if updates.Deliver != nil {
		setClauses = append(setClauses, "deliver = ?")
		args = append(args, *updates.Deliver)
	}
	if updates.DeliverMode != nil {
		setClauses = append(setClauses, "deliver_mode = ?")
		args = append(args, *updates.DeliverMode)
	}
	// SCHED-GAP-1586: parent lane reference with cycle validation — a lane
	// can never become its own ancestor. Self-parenting and any parent
	// chain that loops back to this lane are refused; a parent that names
	// a lane that does not exist is tolerated (dangling reference, the
	// resolver treats the child as a root-level orphan). "" clears back to
	// a primary/root lane.
	if updates.Parent != nil {
		if err := validateParentReference(ctx, db, name, *updates.Parent); err != nil {
			return err
		}
		setClauses = append(setClauses, "parent = ?")
		args = append(args, *updates.Parent)
	}
	if updates.Enabled != nil {
		setClauses = append(setClauses, "enabled = ?")
		args = append(args, boolToInt(*updates.Enabled))
	}
	if updates.DisabledAt != nil {
		setClauses = append(setClauses, "disabled_at = ?")
		args = append(args, *updates.DisabledAt)
	}
	if updates.DisabledBy != nil {
		setClauses = append(setClauses, "disabled_by = ?")
		args = append(args, *updates.DisabledBy)
	}
	if updates.DisabledReason != nil {
		setClauses = append(setClauses, "disabled_reason = ?")
		args = append(args, *updates.DisabledReason)
	}
	if updates.AdaptiveCooldown != nil {
		setClauses = append(setClauses, "adaptive_cooldown = ?")
		args = append(args, boolToInt(*updates.AdaptiveCooldown))
	}
	if updates.CooldownFloorS != nil {
		setClauses = append(setClauses, "cooldown_floor_s = ?")
		args = append(args, *updates.CooldownFloorS)
	}
	if updates.CooldownCeilingS != nil {
		setClauses = append(setClauses, "cooldown_ceiling_s = ?")
		args = append(args, *updates.CooldownCeilingS)
	}
	if updates.NoProgressThreshold != nil {
		setClauses = append(setClauses, "no_progress_threshold = ?")
		args = append(args, *updates.NoProgressThreshold)
	}
	// SCHED-GAP-124: admission mode override with validation — "" clears
	// back to namespace inheritance; otherwise only cooldown|tasks.
	if updates.AdmissionMode != nil {
		switch m := *updates.AdmissionMode; m {
		case "", AdmissionModeCooldown, AdmissionModeTasks:
			setClauses = append(setClauses, "admission_mode = ?")
			args = append(args, m)
		default:
			return fmt.Errorf("invalid admission_mode %q for project %q (want \"\", \"cooldown\" or \"tasks\")", m, name)
		}
	}

	// SCHED-GAP-141: board ownership override with validation — "" restores
	// the derived rule; otherwise only owner|shared.
	if updates.BoardOwnership != nil {
		switch o := *updates.BoardOwnership; o {
		case BoardOwnershipAuto, BoardOwnershipOwner, BoardOwnershipShared:
			setClauses = append(setClauses, "board_ownership = ?")
			args = append(args, o)
		default:
			return fmt.Errorf("invalid board_ownership %q for project %q (want \"\", \"owner\" or \"shared\")", o, name)
		}
	}

	// SCHED-GAP-219: cooldown pin writes. The pin is the durable operator
	// floor: set/raise only (never silently lowered), provenance stamped,
	// and the live cooldown snapped UP to the pin when it sits below so the
	// pin takes effect on the very next admission decision.
	if updates.ClearCooldownPin != nil && *updates.ClearCooldownPin {
		if updates.CooldownPinS != nil {
			return fmt.Errorf("clear_cooldown_pin and cooldown_pin_s are mutually exclusive for project %q", name)
		}
		setClauses = append(setClauses, "cooldown_pin_s = NULL", "cooldown_pin_by = ''", "cooldown_pin_at = ''")
	}
	if updates.CooldownPinS != nil {
		pin := *updates.CooldownPinS
		if pin <= 0 {
			return fmt.Errorf("cooldown_pin_s must be > 0 for project %q (got %d; clear the pin with clear_cooldown_pin=true)", name, pin)
		}
		var curPin sql.NullInt64
		var curCD int
		if err := db.QueryRowContext(ctx,
			`SELECT cooldown_pin_s, cooldown_s FROM projects WHERE name = ?`, name,
		).Scan(&curPin, &curCD); err != nil {
			return fmt.Errorf("read current pin for %q: %w", name, err)
		}
		if curPin.Valid && pin < int(curPin.Int64) {
			// Never silently LOWER an operator pin. An explicit clear +
			// re-set is the sanctioned path to a smaller value.
			return fmt.Errorf("cooldown_pin_s %d would LOWER the existing pin %d for project %q — clear the pin (clear_cooldown_pin=true) first if the lower value is intended", pin, int(curPin.Int64), name)
		}
		now := nowUTC(ctx)
		setClauses = append(setClauses, "cooldown_pin_s = ?", "cooldown_pin_by = ?", "cooldown_pin_at = ?")
		args = append(args, pin, "api", now)
		if curCD < pin {
			// Snap the live cooldown up to the pin so it takes effect
			// immediately; a pin below the live cooldown is a floor, not
			// a forced slowdown.
			setClauses = append(setClauses, "cooldown_s = ?")
			args = append(args, pin)
		}
	}

	args = append(args, name)
	q := "UPDATE projects SET " + strings.Join(setClauses, ", ") + " WHERE name = ?"

	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("update project %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for update project %q: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, name)
	}
	return nil
}

// DeleteProject soft-deletes a project by setting enabled=0. The row is
// retained so historical ticks stay referentially valid.
//
// GAP-044: the soft-delete also stamps disable provenance. COALESCE keeps
// any existing provenance (e.g. a prior pause) and backfills legacy rows
// disabled before migration v12 (ch-delta class) — the API guard only
// allows deleting already-disabled projects, so this path never sees an
// enabled→disabled transition and must write provenance itself.
func DeleteProject(ctx context.Context, db *sql.DB, name string) error {
	now := nowUTC(ctx)
	_, err := db.ExecContext(ctx,
		`UPDATE projects SET enabled = 0,
		   disabled_at = COALESCE(disabled_at, ?),
		   disabled_by = COALESCE(disabled_by, 'api-delete'),
		   disabled_reason = COALESCE(disabled_reason, 'soft-deleted via DELETE ?confirm=true'),
		   updated_at = ?
		 WHERE name = ?`,
		now, now, name)
	if err != nil {
		return fmt.Errorf("delete project %q: %w", name, err)
	}
	return nil
}

// PurgeProject permanently removes a project row from the projects table
// (hard delete — DOGFOOD-009). Historical ticks are retained: they reference
// projects by name string, and the failure-rate breakdown in /api/v1/status
// filters to existing projects, so a purged project's ticks never resurface
// as ghosts.
//
// The ticks.project_name foreign key (NO ACTION) would normally block the
// DELETE while historical ticks exist. Purge therefore disables FK
// enforcement for the duration of the DELETE on the single shared
// connection (SetMaxOpenConns(1) makes this race-free) and restores it via
// defer — the same semantics as the documented manual hard-delete SQL.
func PurgeProject(ctx context.Context, db *sql.DB, name string) error {
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("purge project %q: disable foreign keys: %w", name, err)
	}
	defer func() { _, _ = db.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()

	res, err := db.ExecContext(ctx, `DELETE FROM projects WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("purge project %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("purge project %q: rows affected: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrProjectNotFound, name)
	}
	return nil
}

// boolToInt converts a bool to SQLite's INTEGER representation.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CooldownPinImportBy is the cooldown_pin_by value stamped on pins imported
// from fleet.toml by the boot loader (SCHED-GAP-219) or by the v38 migration
// backfill. Distinct from "api" so an operator can always tell where a pin
// came from.
const CooldownPinImportBy = "fleet-toml-import"

// SetCooldownPin imports an operator pin onto the project row WITHOUT
// touching the live cooldown_s (unlike the API PUT path, which snaps the
// cooldown up). This is the loader/import path: the pin records the
// operator intent discovered in fleet.toml; the cooldown the row already
// carries IS that value in the common case, and when it differs the pin
// still must not silently rewrite scheduling state at boot (the pin is a
// floor, enforced at the next write). Returns the project's post-write pin.
// A pin <= 0 clears nothing — pass zero pins only from paths that already
// validated.
func SetCooldownPin(ctx context.Context, db *sql.DB, name string, pinS int, by string) error {
	if pinS <= 0 {
		return fmt.Errorf("cooldown pin for %q must be > 0 (got %d)", name, pinS)
	}
	if by == "" {
		by = CooldownPinImportBy
	}
	_, err := db.ExecContext(ctx, `
UPDATE projects SET
    cooldown_pin_s = ?,
    cooldown_pin_by = ?,
    cooldown_pin_at = ?,
    updated_at = updated_at
WHERE name = ?`, pinS, by, nowUTC(ctx), name)
	if err != nil {
		return fmt.Errorf("set cooldown pin %q: %w", name, err)
	}
	return nil
}

// BoolPtr returns a pointer to b — a convenience for ProjectUpdates callers.
func BoolPtr(b bool) *bool { return &b }

// IsEmpty reports whether no field in updates is set. The loader uses it to
// skip the UPDATE entirely for rows whose fleet.toml entry carries no
// conditional keys — an empty UpdateProject would still refresh updated_at
// and churn 130+ rows on every boot.
func (u *ProjectUpdates) IsEmpty() bool {
	return u.RepoURL == nil && u.Workdir == nil && u.Weight == nil &&
		u.Priority == nil && u.CooldownS == nil && u.TargetRunsPerDay == nil && u.DecayRate == nil &&
		u.Model == nil && u.Provider == nil &&
		u.FallbackModel == nil && u.FallbackProvider == nil &&
		u.NoGlobalFallback == nil && u.IdleModel == nil && u.IdleProvider == nil &&
		u.DailyBudgetUSD == nil && u.WeeklyBudgetUSD == nil && u.FinalBudgetUSD == nil &&
		u.WorkerModel == nil && u.WorkerProvider == nil && u.GatewayKey == nil &&
		u.Command == nil && u.Prompt == nil && u.PromptMode == nil &&
		u.NamespaceID == nil && u.ModelChain == nil && u.Deliver == nil &&
		u.DeliverMode == nil && u.Parent == nil &&
		u.Enabled == nil && u.DisabledAt == nil && u.DisabledBy == nil &&
		u.DisabledReason == nil && u.AdaptiveCooldown == nil &&
		u.CooldownFloorS == nil && u.CooldownCeilingS == nil &&
		u.NoProgressThreshold == nil && u.AdmissionMode == nil &&
		u.BoardOwnership == nil && u.CooldownPinS == nil && u.ClearCooldownPin == nil
}

// ---------------------------------------------------------------------------
// Task bump (SCHED-GAP-107)
// ---------------------------------------------------------------------------

// Bump limits and defaults. The 6h cooldown law floor applies — 7200 is the
// minimum bump cooldown (killer-lane floor), and a bump may run at most 8
// ticks before auto-revert.
const (
	DefaultBumpTicks    = 5
	DefaultBumpCooldown = 7200
	MaxBumpTicks        = 8
	MinBumpCooldown     = 7200
)

// BumpProject activates a project-wide bump: the project runs at the bump
// cooldown for ticks completed ticks, then auto-reverts (Phase A restore +
// Phase B adaptive re-evaluation — see the scheduler's tick completion hook).
// The pre-bump cooldown policy (cooldown_s, floor, ceiling, no-progress
// streak) is snapshotted into the bump_saved_* columns FIRST so the revert
// is exact — a bump can never permanently ratchet cooldown state. The caller
// is responsible for validation (reason non-empty, ticks 1..8, cooldown >=
// MinBumpCooldown, project enabled, no active bump); this function is
// mechanical. Returns the updated project.
func BumpProject(ctx context.Context, db *sql.DB, name string, ticks, cooldown int, reason string) (*Project, error) {
	now := nowUTC(ctx)
	res, err := db.ExecContext(ctx, `
UPDATE projects SET
	bump_active = 1,
	bump_remaining_ticks = ?,
	bump_cooldown_s = ?,
	bump_reason = ?,
	bump_saved_cooldown_s = COALESCE(cooldown_s, 0),
	bump_saved_floor_s = COALESCE(cooldown_floor_s, 0),
	bump_saved_ceiling_s = COALESCE(cooldown_ceiling_s, 0),
	bump_saved_no_progress_ticks = COALESCE(no_progress_ticks, 0),
	bump_started_at = ?,
	cooldown_s = ?,
	updated_at = ?
WHERE name = ? AND COALESCE(bump_active, 0) = 0`,
		ticks, cooldown, reason, now, cooldown, now, name)
	if err != nil {
		return nil, fmt.Errorf("bump project %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("bump project %q: rows affected: %w", name, err)
	}
	if n == 0 {
		// Either the project is missing or a bump is already active —
		// distinguish for the caller's error mapping.
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM projects WHERE name = ?`, name).Scan(&exists); err != nil {
			return nil, fmt.Errorf("bump project %q: existence check: %w", name, err)
		}
		if exists == 0 {
			return nil, fmt.Errorf("%w: %s", ErrProjectNotFound, name)
		}
		return nil, fmt.Errorf("bump project %q: a bump is already active — wait for it to expire or clear it first", name)
	}
	return GetProject(ctx, db, name)
}

// ErrNoActiveBump is returned by ClearBump when the project has no active
// bump to clear.
var ErrNoActiveBump = errors.New("no active bump")

// ClearBump performs Phase A of the bump revert ONLY: restore the saved
// pre-bump state and clear all bump fields. Phase B — re-running the normal
// adaptive evaluation over the current tick outcome — belongs to the caller
// (the scheduler's tick completion hook), which invokes adaptiveCooldown
// AFTER this restore so the evaluation sees the correct baseline. Manual
// clears (operator aborting a bump via API) intentionally skip Phase B: the
// restored pre-bump state IS the desired end state for an explicit cancel.
func ClearBump(ctx context.Context, db *sql.DB, name string) error {
	res, err := db.ExecContext(ctx, `
UPDATE projects SET
	cooldown_s = CASE WHEN bump_saved_cooldown_s > 0 THEN bump_saved_cooldown_s ELSE COALESCE(cooldown_s, 0) END,
	cooldown_floor_s = bump_saved_floor_s,
	cooldown_ceiling_s = bump_saved_ceiling_s,
	no_progress_ticks = bump_saved_no_progress_ticks,
	bump_active = 0,
	bump_remaining_ticks = 0,
	bump_cooldown_s = 0,
	bump_reason = '',
	bump_saved_cooldown_s = 0,
	bump_saved_floor_s = 0,
	bump_saved_ceiling_s = 0,
	bump_saved_no_progress_ticks = 0,
	bump_started_at = '',
	updated_at = ?
WHERE name = ? AND COALESCE(bump_active, 0) = 1`,
		nowUTC(ctx), name)
	if err != nil {
		return fmt.Errorf("clear bump %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("clear bump %q: rows affected: %w", name, err)
	}
	if n == 0 {
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM projects WHERE name = ?`, name).Scan(&exists); err != nil {
			return fmt.Errorf("clear bump %q: existence check: %w", name, err)
		}
		if exists == 0 {
			return fmt.Errorf("%w: %s", ErrProjectNotFound, name)
		}
		return ErrNoActiveBump
	}
	return nil
}

// ---------------------------------------------------------------------------
// Lane hierarchy (SCHED-GAP-1586)
// ---------------------------------------------------------------------------

// ErrLaneCycle is returned when a parent update would make a lane its own
// ancestor — self-parenting (a→a) and any longer loop (a→b→c→a) both land
// here, so the stored parent graph is always a forest.
var ErrLaneCycle = errors.New("lane parent reference would create a cycle")

// validateParentReference rejects a parent update that would put the lane
// under its own subtree. The child must exist (UpdateProject has already
// proven that by the time this runs); the parent may name a non-existent
// lane (dangling references are tolerated — soft-deleted and purged lanes
// must never block a satellite's reparent) and the walk stops at dangling
// or self-referential EXISTING links rather than looping forever.
func validateParentReference(ctx context.Context, db *sql.DB, child, parent string) error {
	if parent == "" {
		return nil // "" clears back to a primary/root lane — never a cycle
	}
	if parent == child {
		return fmt.Errorf("%w: %q cannot be its own parent", ErrLaneCycle, child)
	}
	const maxLaneDepth = 4096 // defensive bound; far above any real nesting
	seen := map[string]bool{child: true}
	cur := parent
	for i := 0; i < maxLaneDepth; i++ {
		if cur == "" || seen[cur] {
			if seen[cur] {
				return fmt.Errorf("%w: setting parent of %q to %q loops back through %q", ErrLaneCycle, child, parent, cur)
			}
			return nil // dangling parent name — tolerated (resolved as a root child)
		}
		seen[cur] = true
		var next string
		err := db.QueryRowContext(ctx, `SELECT COALESCE(parent, '') FROM projects WHERE name = ?`, cur).Scan(&next)
		if err == sql.ErrNoRows {
			return nil // dangling parent name — tolerated (resolved as a root child)
		}
		if err != nil {
			return fmt.Errorf("parent cycle check for %q: %w", child, err)
		}
		cur = next
	}
	return fmt.Errorf("%w: parent chain from %q exceeds %d hops", ErrLaneCycle, parent, maxLaneDepth)
}

// LaneTree is the resolved lane hierarchy over one snapshot of the projects
// table. This is the ONE implementation later surfaces consume (1587 tree,
// 1590 nesting-in-lists, 1595 parent pages) — do not fork it per surface.
// Nesting depth is a property of a node's position (root = level 0, each
// child = parent + 1); consumers compute it during their own traversal.
type LaneTree struct {
	// Roots holds every lane with no parent ('' parent), plus any orphan
	// whose parent name does not resolve within this snapshot (dangling
	// references survive soft-deletes and purges by design). Ordered by
	// lane name ASC.
	Roots []*LaneNode
}

// LaneNode is one lane in the tree with its children attached.
type LaneNode struct {
	Project Project
	// Children are this lane's satellites, ordered by lane name ASC.
	Children []*LaneNode
}

// BuildLaneTree resolves the lane hierarchy from the given lane list (pass
// ListProjects' enabledOnly=false listing so disabled lanes keep their
// position in the tree). Arbitrary depth is allowed — a satellite may itself
// have satellites. The parent graph is a forest by construction (every write
// is cycle-checked in validateParentReference), and this resolver stays safe
// on top of corrupted/out-of-band data too: a lane whose parent resolves
// within the snapshot but is itself unreachable from a root (a data-level
// cycle) is simply excluded from the tree rather than hung or looped on.
// Roots and every Children slice are ordered by lane name ASC, so the output
// is deterministic regardless of input order.
func BuildLaneTree(projects []Project) *LaneTree {
	byName := make(map[string]*LaneNode, len(projects))
	for i := range projects {
		byName[projects[i].Name] = &LaneNode{Project: projects[i]}
	}

	// attach[parentName] = children pointing at that in-snapshot parent.
	attach := make(map[string][]*LaneNode, len(projects))
	for _, p := range projects {
		if p.Parent == "" {
			continue
		}
		if _, ok := byName[p.Parent]; !ok {
			continue // dangling parent (purged/soft-deleted lane) — surfaces as a root-level orphan
		}
		if p.Name == p.Parent {
			continue // corrupted self-link in data — never self-attach
		}
		attach[p.Parent] = append(attach[p.Parent], byName[p.Name])
	}
	// Deterministic order: lane name ASC within every children slice.
	for k := range attach {
		kids := attach[k]
		sort.Slice(kids, func(i, j int) bool { return kids[i].Project.Name < kids[j].Project.Name })
		attach[k] = kids
	}

	// Roots: no parent, or a parent that does not resolve in this snapshot
	// (dangling reference — the lane is the top of its fragment).
	var roots []*LaneNode
	for _, p := range projects {
		if p.Parent == "" {
			roots = append(roots, byName[p.Name])
			continue
		}
		if _, ok := byName[p.Parent]; !ok {
			roots = append(roots, byName[p.Name])
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Project.Name < roots[j].Project.Name })

	// Hang children depth-first. attach edges only point DOWN the tree
	// from a root, and the write path forbids cycles, so this terminates;
	// data-level cycles never reach here because neither participant is a
	// root (see the exclusion note in the doc comment).
	var hang func(parent *LaneNode)
	hang = func(parent *LaneNode) {
		for _, kid := range attach[parent.Project.Name] {
			parent.Children = append(parent.Children, kid)
			hang(kid)
		}
	}
	for _, r := range roots {
		hang(r)
	}
	return &LaneTree{Roots: roots}
}
