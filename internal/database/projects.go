package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrProjectNotFound is returned when a project lookup or update targets a
// name that does not exist in the projects table.
var ErrProjectNotFound = errors.New("project not found")

// nowUTC returns the current time as a UTC RFC3339 string — the canonical
// timestamp format stored in all TEXT timestamp columns.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// CreateProject inserts a new project row. CreatedAt and UpdatedAt are set
// to the current UTC time if the caller left them zero-valued.
func CreateProject(ctx context.Context, db *sql.DB, p *Project) error {
	if p.CreatedAt == "" {
		p.CreatedAt = nowUTC()
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
(name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, enabled, created_at, updated_at, adaptive_cooldown, cooldown_floor_s, cooldown_ceiling_s, no_progress_threshold, no_progress_ticks, board_rows_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	// A zero-valued BoardRowsSeen on a brand-new row would read as "board
	// observed with 0 rows"; store the unseen sentinel instead so the first
	// adaptive observation only ever establishes a baseline.
	boardRowsSeen := p.BoardRowsSeen
	if boardRowsSeen == 0 {
		boardRowsSeen = AdaptiveUnseenBoardRows
	}
	_, err := db.ExecContext(ctx, q,
		p.Name, p.RepoURL, p.Workdir, p.Weight, p.Priority, p.CooldownS,
		p.DecayRate, p.Model, p.Provider, p.FallbackModel, p.FallbackProvider, boolToInt(p.NoGlobalFallback),
		p.IdleModel, p.IdleProvider,
		p.DailyBudgetUSD, p.WeeklyBudgetUSD, p.FinalBudgetUSD,
		p.WorkerModel, p.WorkerProvider, p.GatewayKey, p.Command, p.Prompt, p.PromptMode, p.NamespaceID, p.Deliver, boolToInt(p.Enabled),
		p.CreatedAt, p.UpdatedAt,
		boolToInt(p.AdaptiveCooldown), p.CooldownFloorS, p.CooldownCeilingS, p.NoProgressThreshold, p.NoProgressTicks, boardRowsSeen)
	if err != nil {
		return fmt.Errorf("create project %q: %w", p.Name, err)
	}
	return nil
}

// GetProject loads a single project by name. Returns ErrProjectNotFound if
// no row matches.
func GetProject(ctx context.Context, db *sql.DB, name string) (*Project, error) {
	const q = `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, enabled, created_at, updated_at, consecutive_failures, COALESCE(last_tick_started, ''), COALESCE(last_tick_completed, ''), COALESCE(disabled_at, ''), COALESCE(disabled_by, ''), COALESCE(disabled_reason, ''), COALESCE(adaptive_cooldown, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0), COALESCE(no_progress_threshold, 0), COALESCE(no_progress_ticks, 0), COALESCE(board_rows_seen, -1)
FROM projects WHERE name = ?`
	var p Project
	var enabled int
	var nsID sql.NullString
	err := db.QueryRowContext(ctx, q, name).Scan(
		&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
		&p.DecayRate, &p.Model, &p.Provider, &p.FallbackModel, &p.FallbackProvider, &p.NoGlobalFallback, &p.IdleModel, &p.IdleProvider,
		&p.DailyBudgetUSD, &p.WeeklyBudgetUSD, &p.FinalBudgetUSD,
		&p.WorkerModel, &p.WorkerProvider, &p.GatewayKey, &p.Command, &p.Prompt, &p.PromptMode, &nsID, &p.Deliver, &enabled, &p.CreatedAt, &p.UpdatedAt, &p.ConsecutiveFailures, &p.LastTickStarted, &p.LastTickCompleted, &p.DisabledAt, &p.DisabledBy, &p.DisabledReason,
		&p.AdaptiveCooldown, &p.CooldownFloorS, &p.CooldownCeilingS, &p.NoProgressThreshold, &p.NoProgressTicks, &p.BoardRowsSeen)
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
	return &p, nil
}

// ListProjects returns projects. If enabledOnly is true, only enabled=1
// rows are returned. Results are ordered by name for stable output.
func ListProjects(ctx context.Context, db *sql.DB, enabledOnly bool) ([]Project, error) {
	q := `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, enabled, created_at, updated_at, consecutive_failures, COALESCE(last_tick_started, ''), COALESCE(last_tick_completed, ''), COALESCE(disabled_at, ''), COALESCE(disabled_by, ''), COALESCE(disabled_reason, ''), COALESCE(adaptive_cooldown, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0), COALESCE(no_progress_threshold, 0), COALESCE(no_progress_ticks, 0), COALESCE(board_rows_seen, -1)
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
		if err := rows.Scan(
			&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
			&p.DecayRate, &p.Model, &p.Provider, &p.FallbackModel, &p.FallbackProvider, &p.NoGlobalFallback, &p.IdleModel, &p.IdleProvider,
			&p.DailyBudgetUSD, &p.WeeklyBudgetUSD, &p.FinalBudgetUSD,
			&p.WorkerModel, &p.WorkerProvider, &p.GatewayKey, &p.Command, &p.Prompt, &p.PromptMode, &nsID, &p.Deliver, &enabled,
			&p.CreatedAt, &p.UpdatedAt, &p.ConsecutiveFailures, &p.LastTickStarted, &p.LastTickCompleted, &p.DisabledAt, &p.DisabledBy, &p.DisabledReason,
			&p.AdaptiveCooldown, &p.CooldownFloorS, &p.CooldownCeilingS, &p.NoProgressThreshold, &p.NoProgressTicks, &p.BoardRowsSeen); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.Enabled = enabled != 0
		if nsID.Valid {
			p.NamespaceID = &nsID.String
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
	q := `SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate, model, provider, fallback_model, fallback_provider, no_global_fallback, idle_model, idle_provider, daily_budget_usd, weekly_budget_usd, final_budget_usd, worker_model, worker_provider, gateway_key, command, prompt, prompt_mode, namespace_id, deliver, enabled, created_at, updated_at, consecutive_failures, COALESCE(last_tick_started, ''), COALESCE(last_tick_completed, ''), COALESCE(disabled_at, ''), COALESCE(disabled_by, ''), COALESCE(disabled_reason, ''), COALESCE(adaptive_cooldown, 0), COALESCE(cooldown_floor_s, 0), COALESCE(cooldown_ceiling_s, 0), COALESCE(no_progress_threshold, 0), COALESCE(no_progress_ticks, 0), COALESCE(board_rows_seen, -1)
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
		if err := rows.Scan(
			&p.Name, &p.RepoURL, &p.Workdir, &p.Weight, &p.Priority, &p.CooldownS,
			&p.DecayRate, &p.Model, &p.Provider, &p.FallbackModel, &p.FallbackProvider, &p.NoGlobalFallback, &p.IdleModel, &p.IdleProvider,
			&p.DailyBudgetUSD, &p.WeeklyBudgetUSD, &p.FinalBudgetUSD,
			&p.WorkerModel, &p.WorkerProvider, &p.GatewayKey, &p.Command, &p.Prompt, &p.PromptMode, &nsID, &p.Deliver, &enabled,
			&p.CreatedAt, &p.UpdatedAt, &p.ConsecutiveFailures, &p.LastTickStarted, &p.LastTickCompleted, &p.DisabledAt, &p.DisabledBy, &p.DisabledReason,
			&p.AdaptiveCooldown, &p.CooldownFloorS, &p.CooldownCeilingS, &p.NoProgressThreshold, &p.NoProgressTicks, &p.BoardRowsSeen); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		p.Enabled = enabled != 0
		if nsID.Valid {
			p.NamespaceID = &nsID.String
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
	Enabled          *bool    `json:"enabled"`
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
	args := []any{nowUTC()}

	// GAP-044: resolve the enabled transition before building clauses.
	if updates.Enabled != nil {
		var curEnabled int
		err := db.QueryRowContext(ctx,
			`SELECT enabled FROM projects WHERE name = ?`, name).Scan(&curEnabled)
		if err != nil {
			return fmt.Errorf("read current enabled for %q: %w", name, err)
		}
		now := nowUTC()
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
			// Fresh enablement — reset the runtime streak.
			setClauses = append(setClauses,
				"no_progress_ticks = 0", "board_rows_seen = -1")
			if updates.CooldownFloorS == nil && curCD > 0 {
				floor := curCD
				updates.CooldownFloorS = &floor
			}
			if updates.CooldownCeilingS == nil {
				ceiling := DefaultAdaptiveCooldownCeilingS
				updates.CooldownCeilingS = &ceiling
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
	now := nowUTC()
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

// BoolPtr returns a pointer to b — a convenience for ProjectUpdates callers.
func BoolPtr(b bool) *bool { return &b }
