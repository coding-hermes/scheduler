package database

import "encoding/json"

// Project is a single managed codebase the scheduler may spawn ticks against.
// Field ordering matches the projects table column order for scan ergonomics.
type Project struct {
	Name              string  `json:"name"`                // PRIMARY KEY — also the DuckBrain project key
	RepoURL           string  `json:"repo_url"`            // git clone URL
	Workdir           string  `json:"workdir"`             // absolute path to the working copy on this host
	Weight            int     `json:"weight"`              // 1..100 — weight budget consumed per tick (default 10)
	Priority          int     `json:"priority"`            // 1..10 — base urgency multiplier (default 5)
	CooldownS         int     `json:"cooldown_s"`          // seconds between successive ticks (default 900)
	DecayRate         float64 `json:"decay_rate"`          // urgency decay rate (default 1.0)
	Model             string  `json:"model"`               // LLM model id passed to the spawned agent
	Provider          string  `json:"provider"`            // LLM provider id passed to the spawned agent
	FallbackModel     string  `json:"fallback_model"`      // optional: fallback model tier for the spawn chain (SCHED-GAP-064)
	FallbackProvider  string  `json:"fallback_provider"`   // optional: fallback provider tier for the spawn chain (SCHED-GAP-064)
	NoGlobalFallback  bool    `json:"no_global_fallback"`  // true → skip the spawner-level (env) fallback tier entirely (SCHED-GAP-064)
	ModelChain        string  `json:"model_chain"`         // ordered list of "model@provider" hops (JSON array); empty = use model/provider + fallback_model/provider (SCHED-GAP-075)
	IdleModel         string  `json:"idle_model"`          // optional: idle-tick model tier, prepended to the spawn chain when the board has zero pending tasks (SCHED-GAP-065)
	IdleProvider      string  `json:"idle_provider"`       // optional: idle-tick provider tier (SCHED-GAP-065)
	DailyBudgetUSD    float64 `json:"daily_budget_usd"`    // per-UTC-day spend cap; <= 0 = unlimited (SCHED-GAP-066)
	WeeklyBudgetUSD   float64 `json:"weekly_budget_usd"`   // per-UTC-week spend cap (Monday 00:00 UTC reset); <= 0 = unlimited (SCHED-GAP-066)
	FinalBudgetUSD    float64 `json:"final_budget_usd"`    // one-time lifetime spend cap, never resets; <= 0 = unlimited (SCHED-GAP-066)
	WorkerModel       string  `json:"worker_model"`        // optional: suggested worker model (foreman can override)
	WorkerProvider    string  `json:"worker_provider"`     // optional: suggested worker provider (foreman can override)
	GatewayKey        string  `json:"gateway_key"`         // per-foreman Hermes gateway key; empty = use daemon's shared --gateway-key
	Command           string  `json:"command"`             // optional: custom spawn command (overrides default hermes chat)
	Prompt            string  `json:"prompt"`              // optional: extra foreman prompt text; appended to the namespace default_prompt unless PromptMode=replace (Bane 2026-08-27)
	PromptMode        string  `json:"prompt_mode"`         // "append" (default): project prompt appends to namespace default; "replace": project prompt replaces it entirely
	NamespaceID       *string `json:"namespace_id"`        // optional: FK → namespaces.id; NULL = unscheduled in namespace mode
	Deliver           string  `json:"deliver"`             // delivery target: platform:chat_id:thread_id (e.g. telegram:-1003310984808:12)
	Enabled           bool    `json:"enabled"`             // disabled projects are never scheduled
	CreatedAt         string  `json:"created_at"`          // RFC3339 timestamp
	UpdatedAt         string  `json:"updated_at"`          // RFC3339 timestamp
	LastTickStarted   string  `json:"last_tick_started"`   // RFC3339 of most recent tick spawn; "" when never spawned
	LastTickCompleted string  `json:"last_tick_completed"` // RFC3339 of most recent tick completion (any outcome); "" when never completed

	// Disable provenance (GAP-044): who disabled the project, when, and
	// why. All empty when the project has never been disabled (or was
	// re-enabled). Written by every disable path (API pause/PUT/DELETE,
	// auto-disable) so fleet-state changes stay auditable.
	DisabledAt     string `json:"disabled_at"`     // RFC3339 when disabled; "" = enabled/never disabled
	DisabledBy     string `json:"disabled_by"`     // "api" | "api-pause" | "api-delete" | "auto-disable"
	DisabledReason string `json:"disabled_reason"` // human-readable why (failure stats for auto-disable)

	// ConsecutiveFailures counts consecutive SPAWN failures (gateway
	// unreachable, process start error). Incremented by Spawner.Spawn on
	// failure, reset to 0 on the first successful spawn. Drives the
	// exponential selection backoff (S-GAP-001). Internal scheduler state —
	// not user-editable via ProjectUpdates.
	ConsecutiveFailures int `json:"consecutive_failures"`
}

// UnmarshalJSON decodes a Project from JSON. Canonical S06 keys are
// snake_case (see the json tags above), but fleet automation deployed before
// the wire-format conformance fix (DOGFOOD-001, 2026-08-04) still sends the
// legacy PascalCase Go field names (Name, RepoURL, CooldownS, …). This
// method first applies the standard tag-based decode, then back-fills any
// zero-valued field from its legacy PascalCase key so both spellings keep
// working. Unknown keys are ignored, matching encoding/json defaults.
func (p *Project) UnmarshalJSON(data []byte) error {
	// Alias avoids infinite recursion through this method.
	type projectAlias Project
	var a projectAlias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*p = Project(a)

	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	setString := func(key string, dst *string) {
		if *dst != "" {
			return
		}
		if raw, ok := legacy[key]; ok {
			_ = json.Unmarshal(raw, dst)
		}
	}
	setInt := func(key string, dst *int) {
		if *dst != 0 {
			return
		}
		if raw, ok := legacy[key]; ok {
			_ = json.Unmarshal(raw, dst)
		}
	}
	setString("Name", &p.Name)
	setString("RepoURL", &p.RepoURL)
	setString("Workdir", &p.Workdir)
	setInt("Weight", &p.Weight)
	setInt("Priority", &p.Priority)
	setInt("CooldownS", &p.CooldownS)
	if p.DecayRate == 0 {
		if raw, ok := legacy["DecayRate"]; ok {
			_ = json.Unmarshal(raw, &p.DecayRate)
		}
	}
	setString("Model", &p.Model)
	setString("Provider", &p.Provider)
	setString("FallbackModel", &p.FallbackModel)
	setString("FallbackProvider", &p.FallbackProvider)
	setString("ModelChain", &p.ModelChain)
	setString("IdleModel", &p.IdleModel)
	setString("IdleProvider", &p.IdleProvider)
	if !p.NoGlobalFallback {
		if raw, ok := legacy["NoGlobalFallback"]; ok {
			var b bool
			if err := json.Unmarshal(raw, &b); err == nil {
				p.NoGlobalFallback = b
			}
		}
	}
	setString("WorkerModel", &p.WorkerModel)
	setString("WorkerProvider", &p.WorkerProvider)
	setString("GatewayKey", &p.GatewayKey)
	setString("Command", &p.Command)
	if p.NamespaceID == nil {
		if raw, ok := legacy["NamespaceID"]; ok {
			var ns string
			if err := json.Unmarshal(raw, &ns); err == nil {
				p.NamespaceID = &ns
			}
		}
	}
	setString("Deliver", &p.Deliver)
	if !p.Enabled {
		if raw, ok := legacy["Enabled"]; ok {
			_ = json.Unmarshal(raw, &p.Enabled)
		}
	}
	setString("CreatedAt", &p.CreatedAt)
	setString("UpdatedAt", &p.UpdatedAt)
	setString("LastTickStarted", &p.LastTickStarted)
	setString("LastTickCompleted", &p.LastTickCompleted)
	setString("DisabledAt", &p.DisabledAt)
	setString("DisabledBy", &p.DisabledBy)
	setString("DisabledReason", &p.DisabledReason)
	return nil
}

// TickStatus enumerates the lifecycle states a tick may occupy.
type TickStatus string

const (
	StatusQueued    TickStatus = "queued"
	StatusRunning   TickStatus = "running"
	StatusCompleted TickStatus = "completed"
	StatusFailed    TickStatus = "failed"
	StatusTimeout   TickStatus = "timeout"
)

// TickOutcome records the terminal result of a tick.
type TickOutcome string

const (
	OutcomeCommitted TickOutcome = "committed"
	OutcomeDryRun    TickOutcome = "dry_run"
	OutcomeFailed    TickOutcome = "failed"
	OutcomeTimeout   TickOutcome = "timeout"
)

// Tick is a single scheduler run: one spawned agent invocation against one
// project, tracked from queue through completion.
type Tick struct {
	ID           string      `json:"id"` // PRIMARY KEY — see NextTickID for format
	ProjectName  string      `json:"project_name"`
	SessionID    string      `json:"session_id"` // captured from spawned process stdout
	Status       TickStatus  `json:"status"`
	Outcome      TickOutcome `json:"outcome"` // set on terminal transition
	SpawnedAt    string      `json:"spawned_at"`
	CompletedAt  string      `json:"completed_at"`
	ExitCode     int         `json:"exit_code"`
	Commits      int         `json:"commits"`
	FilesChanged int         `json:"files_changed"`
	TokensIn     int64       `json:"tokens_in"`
	TokensOut    int64       `json:"tokens_out"`
	CostUSD      float64     `json:"cost_usd"`
	Urgency      float64     `json:"urgency"` // urgency score at spawn time
	WeightUsed   int         `json:"weight_used"`
	Error        string      `json:"error"`
	CreatedAt    string      `json:"created_at"`
}

// EventSeverity enumerates the severity tiers for event log entries.
type EventSeverity string

const (
	SeverityCritical EventSeverity = "CRITICAL"
	SeverityHigh     EventSeverity = "HIGH"
	SeverityMedium   EventSeverity = "MEDIUM"
	SeverityLow      EventSeverity = "LOW"
	SeverityInfo     EventSeverity = "INFO"
)

// Event is a single log line in the operational event log. Decisions and
// errors land here; info captures routine operational notes.
type Event struct {
	ID        int64         `json:"id"` // AUTOINCREMENT PK
	Severity  EventSeverity `json:"severity"`
	Component string        `json:"component"` // system component that emitted the event
	Message   string        `json:"message"`
	Details   string        `json:"details"` // free-form context, often JSON
	CreatedAt string        `json:"created_at"`
}

// Namespace represents a weight pool for related cron jobs.
// Each namespace gets a share of the global budget (B=100) via a two-phase
// allocation algorithm: reserved floor + proportional remainder, capped by hard_cap.
type Namespace struct {
	ID            string `json:"id"`             // PRIMARY KEY — unique slug (e.g. "coding-hermes")
	Weight        int    `json:"weight"`         // 1..100 — relative weight for proportional allocation
	Reserved      int    `json:"reserved"`       // >= 0 — guaranteed floor budget units
	HardCap       int    `json:"hard_cap"`       // >= 0 — maximum budget; 0 means no cap (interpret as B)
	MaxConcurrent int    `json:"max_concurrent"` // >= 0 — max ticks running at once in this namespace; 0 = unlimited (global cap still applies)
	Enabled       bool   `json:"enabled"`        // disabled namespaces get zero allocation
	Description   string `json:"description"`    // human-readable label
	DefaultPrompt string `json:"default_prompt"` // foreman prompt default for every project in this namespace; empty = built-in (Bane 2026-08-27)
	CreatedAt     string `json:"created_at"`     // RFC3339
	UpdatedAt     string `json:"updated_at"`     // RFC3339
}

// NamespacePatch is used for partial updates. Only non-nil fields are applied.
type NamespacePatch struct {
	Weight        *int    `json:"weight,omitempty"`
	Reserved      *int    `json:"reserved,omitempty"`
	HardCap       *int    `json:"hard_cap,omitempty"`
	MaxConcurrent *int    `json:"max_concurrent,omitempty"`
	Enabled       *bool   `json:"enabled,omitempty"`
	Description   *string `json:"description,omitempty"`
	DefaultPrompt *string `json:"default_prompt,omitempty"` // Bane 2026-08-27: namespace foreman prompt default
}

// NamespaceTick records per-namespace utilization for a single evaluation cycle.
type NamespaceTick struct {
	ID          int64  `json:"id"`           // AUTOINCREMENT PK
	TickGroup   string `json:"tick_group"`   // group identifier: <YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>
	NamespaceID string `json:"namespace_id"` // FK → namespaces.id
	Allocated   int    `json:"allocated"`    // budget given this tick
	Used        int    `json:"used"`         // budget actually consumed (sum of effective weights)
	Borrowed    int    `json:"borrowed"`     // extra budget from other namespaces
	Lent        int    `json:"lent"`         // budget given to other namespaces
	JobCount    int    `json:"job_count"`    // how many jobs ran
	CreatedAt   string `json:"created_at"`   // RFC3339
}
