package database

import (
	"encoding/json"
	"fmt"
)

// Adaptive-cooldown built-in defaults (shared by the DB update layer, which
// normalizes rows when the feature is enabled, the TOML loader, and the
// scheduler runtime, which falls back to these when a column is 0).
// Semantics:
//
//   - floor:        the base cooldown a project resets to on ANY progress.
//     0 = unset (normalized to the cooldown_s in force at enable
//     time); for dynamic (cooldown_s = 0) projects it stays 0
//     and adaptive only tracks the streak, never escalates.
//   - ceiling:      escalation cap in seconds. When no explicit
//     cooldown_ceiling_s is set, the default is DERIVED as 8 × the
//     effective floor (ADV-R10) — the same shape as the fleet.toml 8×floor
//     pins. An explicit ceiling always wins. There is NO second hardcoded
//     default constant.
//   - threshold:    consecutive no-progress ticks before escalation begins.
//   - no_progress_ticks: live counter of the consecutive no-progress streak
//     (observable via the API; reset on any progress).
//   - board_rows_seen: last observed tasks.jsonl row count (-1 = never
//     observed) — the "new work" signal for injected board rows.
const (
	DefaultAdaptiveCooldownThreshold = 10
	AdaptiveUnseenBoardRows          = -1 // board_rows_seen sentinel: no baseline yet
)

// AdaptiveCeilingFloorMultiplier is the single authority for the derived
// default adaptive-cooldown ceiling: when a project has no explicit
// cooldown_ceiling_s, the cap is this multiple of the effective cooldown
// floor (ADV-R10). It matches the fleet.toml pin shape
// (cooldown_ceiling_s = 8 × cooldown_floor_s), so a project whose fleet
// entry loses its explicit ceiling derives the same cap the pins carry
// instead of parking at the retired weekly constant.
const AdaptiveCeilingFloorMultiplier = 8

// DefaultAdaptiveCooldownCeiling derives the default adaptive-cooldown
// ceiling for a project with the given floor: 8 × floor (ADV-R10). A
// non-positive floor yields 0 — nothing is derivable, and the runtime
// treats a 0 ceiling on a floorless project as "track the streak, never
// escalate" (callers that still need a bounded cap pass the live cooldown
// as the fallback base).
func DefaultAdaptiveCooldownCeiling(floorS int) int {
	if floorS <= 0 {
		return 0
	}
	return floorS * AdaptiveCeilingFloorMultiplier
}

// Project is a single managed codebase the scheduler may spawn ticks against.
// Field ordering matches the projects table column order for scan ergonomics.
type Project struct {
	Name             string  `json:"name"`               // PRIMARY KEY — also the DuckBrain project key
	RepoURL          string  `json:"repo_url"`           // git clone URL
	Workdir          string  `json:"workdir"`            // absolute path to the working copy on this host
	Weight           int     `json:"weight"`             // 1..100 — weight budget consumed per tick (default 10)
	Priority         int     `json:"priority"`           // 1..10 — base urgency multiplier (default 5)
	CooldownS        int     `json:"cooldown_s"`         // seconds between successive ticks (default 900)
	DecayRate        float64 `json:"decay_rate"`         // urgency decay rate (default 1.0)
	Model            string  `json:"model"`              // LLM model id passed to the spawned agent
	Provider         string  `json:"provider"`           // LLM provider id passed to the spawned agent
	FallbackModel    string  `json:"fallback_model"`     // optional: fallback model tier for the spawn chain (SCHED-GAP-064)
	FallbackProvider string  `json:"fallback_provider"`  // optional: fallback provider tier for the spawn chain (SCHED-GAP-064)
	NoGlobalFallback bool    `json:"no_global_fallback"` // true → skip the spawner-level (env) fallback tier entirely (SCHED-GAP-064)
	ModelChain       string  `json:"model_chain"`        // ordered list of "model@provider" hops (JSON array); empty = use model/provider + fallback_model/provider (SCHED-GAP-075)
	IdleModel        string  `json:"idle_model"`         // optional: idle-tick model tier, prepended to the spawn chain when the board has zero pending tasks (SCHED-GAP-065)
	IdleProvider     string  `json:"idle_provider"`      // optional: idle-tick provider tier (SCHED-GAP-065)
	DailyBudgetUSD   float64 `json:"daily_budget_usd"`   // per-UTC-day spend cap; <= 0 = unlimited (SCHED-GAP-066)
	WeeklyBudgetUSD  float64 `json:"weekly_budget_usd"`  // per-UTC-week spend cap (Monday 00:00 UTC reset); <= 0 = unlimited (SCHED-GAP-066)
	FinalBudgetUSD   float64 `json:"final_budget_usd"`   // one-time lifetime spend cap, never resets; <= 0 = unlimited (SCHED-GAP-066)
	WorkerModel      string  `json:"worker_model"`       // optional: suggested worker model (foreman can override)
	WorkerProvider   string  `json:"worker_provider"`    // optional: suggested worker provider (foreman can override)
	GatewayKey       string  `json:"gateway_key"`        // per-foreman Hermes gateway key; empty = use daemon's shared --gateway-key
	Command          string  `json:"command"`            // optional: custom spawn command (overrides default hermes chat)
	Prompt           string  `json:"prompt"`             // optional: extra foreman prompt text; appended to the namespace default_prompt unless PromptMode=replace (Bane 2026-08-27)
	PromptMode       string  `json:"prompt_mode"`        // "append" (default): project prompt appends to namespace default; "replace": project prompt replaces it entirely
	NamespaceID      *string `json:"namespace_id"`       // optional: FK → namespaces.id; NULL = unscheduled in namespace mode
	Deliver          string  `json:"deliver"`            // delivery target: platform:chat_id:thread_id (e.g. telegram:-1003310984808:12)
	// SCHED-GAP-1607: tick-report delivery mode — full (default) | file | link.
	// '' and unknown values resolve to full, so pre-1607 rows are unchanged.
	DeliverMode string `json:"deliver_mode"` // see scheduler.DeliverMode* constants
	// SCHED-GAP-1586: the name of the lane this lane is a satellite of.
	// '' = a primary/root lane. Arbitrary depth is allowed (a satellite may
	// itself have satellites); writes are cycle-checked (see
	// validateParentReference). NOT a FK: lane names are soft-deleted
	// (enabled=0) or purged (FK off), and a purged parent must not cascade
	// or block — dangling parents are read back verbatim and the resolver
	// (BuildLaneTree) treats a missing parent as a root child.
	Parent            string `json:"parent"`              // parent lane name; '' = primary/root lane
	Enabled           bool   `json:"enabled"`             // disabled projects are never scheduled
	CreatedAt         string `json:"created_at"`          // RFC3339 timestamp
	UpdatedAt         string `json:"updated_at"`          // RFC3339 timestamp
	LastTickStarted   string `json:"last_tick_started"`   // RFC3339 of most recent tick spawn; "" when never spawned
	LastTickCompleted string `json:"last_tick_completed"` // RFC3339 of most recent tick completion (any outcome); "" when never completed
	// SCHED-GAP-214 (migration v38): terminal status of the most recent
	// tick ("" = never ticked | completed | failed | timeout | deferred).
	// Stamped by lifecycle.Complete; read by the tasks-mode cooldown waiver
	// — a FAILED last tick stands the waiver down for one full cooldown.
	LastTickStatus string `json:"last_tick_status"`

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

	// Adaptive-cooldown policy (opt-in per project). When AdaptiveCooldown
	// is false (the default) the legacy autoSlowdown verdict behavior is
	// unchanged. When true, the scheduler counts consecutive no-progress
	// ticks (0 commits AND no new tasks.jsonl rows since the previous tick)
	// and, once the streak reaches NoProgressThreshold, multiplies the
	// effective cooldown (cooldown_s) progressively up to CooldownCeilingS.
	// ANY progress — a non-zero-commit tick or a new board row — resets the
	// streak and drops cooldown_s back to CooldownFloorS (the speed-up
	// path: a UPD-* board wave instantly re-accelerates a parked project).
	// Zero values on the config columns mean "use the built-in default"
	// (see DefaultAdaptiveCooldown*); the update layer normalizes them to
	// effective values whenever the feature transitions to enabled.
	AdaptiveCooldown    bool `json:"adaptive_cooldown"`
	CooldownFloorS      int  `json:"cooldown_floor_s"`
	CooldownCeilingS    int  `json:"cooldown_ceiling_s"`
	NoProgressThreshold int  `json:"no_progress_threshold"`
	NoProgressTicks     int  `json:"no_progress_ticks"`
	BoardRowsSeen       int  `json:"board_rows_seen"`

	// Task bump (SCHED-GAP-107): a temporary project-wide speed-up that
	// runs the project at BumpCooldownS for BumpRemainingTicks completed
	// bump ticks, then auto-reverts through the two-phase restore
	// (Phase A: restore the pre-bump snapshot below; Phase B: re-run the
	// adaptive evaluation over the tick outcome). A bump can never
	// permanently ratchet cooldown state — the saved_* columns hold the
	// pre-bump values so the revert is exact.
	BumpActive          bool   `json:"bump_active"`
	BumpRemainingTicks  int    `json:"bump_remaining_ticks"`
	BumpCooldownS       int    `json:"bump_cooldown_s"`
	BumpReason          string `json:"bump_reason"`
	BumpSavedCooldownS  int    `json:"bump_saved_cooldown_s"`
	BumpSavedFloorS     int    `json:"bump_saved_floor_s"`
	BumpSavedCeilingS   int    `json:"bump_saved_ceiling_s"`
	BumpSavedNoProgress int    `json:"bump_saved_no_progress_ticks"`
	BumpStartedAt       string `json:"bump_started_at"`

	// SCHED-GAP-124: per-project admission-mode override of the
	// namespace's admission_mode. "" (default) = inherit the namespace;
	// "cooldown" = cron admission; "tasks" = work-driven admission
	// (non-perpetual pending board work admits immediately, cooldown
	// pin applies when the board is drained). Validated at write time.
	AdmissionMode string `json:"admission_mode"`

	// SCHED-GAP-141: per-project board-ownership override of the derived
	// rule. "" (default) = auto — the tasks-mode waiver fires only when
	// the board a lane reads resolves inside the lane's own workdir.
	// "owner" = this lane owns the board it reads even when the resolved
	// path lies outside its workdir; "shared" = this lane reads a board
	// it does NOT own (paced by cooldown) even when the path check would
	// pass. Validated at write time.
	BoardOwnership string `json:"board_ownership"`

	// SCHED-GAP-219 cooldown pin provenance. The pin is a durable row
	// attribute: the operator value the cooldown must never silently drop
	// below. NULL (nil) = no pin — the cooldown is policy/discretion
	// territory. Provenance records who set it ("fleet-toml-import" for
	// the migration backfill, "api" for a PUT-set pin) and when.
	// CooldownPinS is a *int so "no pin" and "pinned to 0" stay
	// distinguishable on the wire; a 0 pin is meaningless (cooldowns are
	// positive) and is rejected at the write paths.
	CooldownPinS  *int   `json:"cooldown_pin_s"`
	CooldownPinBy string `json:"cooldown_pin_by"`
	CooldownPinAt string `json:"cooldown_pin_at"`
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
	// SCHED-GAP-214: legacy-PascalCase backfill mirrors the tag decode;
	// the canonical wire form is last_tick_status.
	setString("LastTickStatus", &p.LastTickStatus)
	setString("DisabledAt", &p.DisabledAt)
	setString("DisabledBy", &p.DisabledBy)
	setString("DisabledReason", &p.DisabledReason)
	if !p.AdaptiveCooldown {
		if raw, ok := legacy["AdaptiveCooldown"]; ok {
			_ = json.Unmarshal(raw, &p.AdaptiveCooldown)
		}
	}
	setInt("CooldownFloorS", &p.CooldownFloorS)
	setInt("CooldownCeilingS", &p.CooldownCeilingS)
	setInt("NoProgressThreshold", &p.NoProgressThreshold)
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
	CostSource   string      `json:"cost_source"` // ADV-R09/G8: measured | gateway | estimated | simulated | "" (legacy)
	// SCHED-GAP-176 (surface honesty): both columns are structurally always
	// zero — RecordTickMetrics (internal/database/ticks.go) is their only
	// writer and is dead code, so the REST payload emitted 0 for every one of
	// the 72k+ ticks rows. The columns stay in the schema and the struct
	// fields stay for the dead writer + tests, but nothing serializes them.
	Urgency    float64 `json:"-"`
	WeightUsed int     `json:"-"`
	Error      string  `json:"error"`
	CreatedAt  string  `json:"created_at"`
	// SCHED-GAP-091: session resume-after-restart. OrphanedAt is stamped
	// the moment a previously-running tick's owner (gateway/daemon)
	// is known to be gone; OrphanReason records which drop path fired.
	// NudgeCount counts continuation re-spawns — capped at
	// scheduler.MaxNudgesPerTick so a flapping gateway cannot resurrect
	// a tick forever (anti zombie-resurrection).
	OrphanedAt   string `json:"orphaned_at,omitempty"`
	OrphanReason string `json:"orphan_reason,omitempty"`
	NudgeCount   int    `json:"nudge_count"`
	// SCHED-GAP-104 commit-anatomy signals: the tick's commits split by
	// path into product code vs fleet bookkeeping (.coding-hermes/). Only
	// CodeCommits count as progress for adaptive cooldown.
	CodeCommits  int `json:"code_commits"`
	BoardCommits int `json:"board_commits"`
	// SCHED-GAP-107: 1 when this tick was spawned while the project had an
	// active bump (it ran at bump cooldown and consumes one bump tick).
	// 0 = normal tick. Lets yield analysis compare bump vs normal ticks.
	Bump int `json:"bump"`
	// S12 concurrent wave scheduling (SCHED-GAP-109): worker attribution
	// on the tick row itself.
	WorkerCount  int `json:"worker_count"`  // worker sessions the foreman dispatched inside this tick (0 = serial tick; sessions = 1 + worker_count)
	WaveRecovery int `json:"wave_recovery"` // 1 = this tick ran the wave-recovery phase first (S12 §8.2)
	// SCHED-GAP-157 lifecycle persistence. All three read as honest
	// empties for legacy rows (the columns are NOT NULL DEFAULT 0/''),
	// never a fabricated value: "" = not recorded for this row.
	SlotWaitMs  int64  `json:"slot_wait_ms"` // ms between admission/queueing and the slot being acquired
	AdmitReason string `json:"admit_reason"` // the admission decision that let the tick in (SCHED-GAP-155 vocabulary)
	NudgeSource string `json:"nudge_source"` // why this tick row exists outside the packer: startup | manual | board_wake
}

// Terminal tick statuses for projects.last_tick_status (SCHED-GAP-214,
// migration v38). Stamped by lifecycle.Complete on every terminal outcome;
// "" = the project has never completed a tick. The tasks-mode cooldown
// waiver (admission_mode.go + the three packer paths) reads this field: a
// FAILED last tick stands the waiver down for one full effective cooldown.
const (
	LastStatusCompleted = "completed"
	LastStatusFailed    = "failed"
	LastStatusTimeout   = "timeout"
	LastStatusDeferred  = "deferred"
)

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
	ModelChain    string `json:"model_chain"`    // ordered "model@provider" hops (JSON array); namespace tier between project and router (Bane 2026-08-27)
	// S12 concurrent wave scheduling (SCHED-GAP-109): namespace-level wave
	// config, all default-off. WaveEnabled=false leaves scheduling
	// byte-identical to pre-v27 behavior.
	WaveEnabled     bool   `json:"wave_enabled"`      // true → ticks in this namespace may compose waves (S12 §4)
	WaveTickTimeout string `json:"wave_tick_timeout"` // duration string; "" = inherit scheduler tick timeout (S12 §4)
	WaveWorkersCap  int    `json:"wave_workers_cap"`  // max concurrent worker processes across the namespace's running ticks; 0 = unlimited (S12 §6)
	// SCHED-GAP-124 admission mode: how member projects become eligible
	// for selection. "cooldown" (default) = wall-clock cron semantics —
	// last_tick_completed + cooldown_s gates each tick. "tasks" = work
	// driven — a project with non-perpetual pending board work is
	// admitted immediately (priority + namespace caps still apply);
	// when its board has none, the cooldown pin applies again.
	AdmissionMode string `json:"admission_mode"` // "cooldown" | "tasks"
	// SCHED-GAP-125: load-gate opt-out. "" = gate applies when globally
	// enabled; "off" = this namespace's spawns never defer on load.
	LoadGate  string `json:"load_gate"`  // "" | "off"
	CreatedAt string `json:"created_at"` // RFC3339
	UpdatedAt string `json:"updated_at"` // RFC3339
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
	ModelChain    *string `json:"model_chain,omitempty"`    // namespace model chain (JSON array string)
	// S12 wave config (SCHED-GAP-109): applied only when non-nil, same as
	// every other field above.
	WaveEnabled     *bool   `json:"wave_enabled,omitempty"`      // namespace wave switch (default off)
	WaveTickTimeout *string `json:"wave_tick_timeout,omitempty"` // "" = inherit scheduler tick timeout
	WaveWorkersCap  *int    `json:"wave_workers_cap,omitempty"`  // 0 = unlimited
	// SCHED-GAP-124: admission mode override; must be "cooldown" or
	// "tasks" when non-nil (validated by UpdateNamespace).
	AdmissionMode *string `json:"admission_mode,omitempty"`
	// SCHED-GAP-125: load-gate opt-out; must be "off" when non-nil
	// (validated by UpdateNamespace). Nil/empty = gate applies.
	LoadGate *string `json:"load_gate,omitempty"`
}

// TickWorker is one dispatched worker inside a wave tick (S12 §9.2,
// SCHED-GAP-109). One row per worker session the foreman reports in its
// wave manifest: which task, which branch, which judge verdict, which
// merge outcome. Rows are created at manifest ingestion (SCHED-GAP-110)
// and closed by the foreman or the reaper (SCHED-GAP-114).
//
// W4 MONEY INVARIANT: CostUSD is ATTRIBUTION ONLY — the worker sessions
// already run in the foreman's Hermes home, so resolveRealTickCost has
// already counted them inside ticks.cost_usd. TickWorker.CostUSD is never
// added to ticks.cost_usd or any fleet total; it exists solely to split a
// tick's cost per task/branch in reporting (SCHED-GAP-115).
type TickWorker struct {
	ID        int64   `json:"id"`         // AUTOINCREMENT PK
	TickID    string  `json:"tick_id"`    // FK → ticks.id (ON DELETE CASCADE)
	TaskID    string  `json:"task_id"`    // board task the worker was dispatched for
	Branch    string  `json:"branch"`     // wt/<task-id>
	Worktree  string  `json:"worktree"`   // absolute worktree path; "" when unreported
	CommitSHA string  `json:"commit_sha"` // branch tip reported by the foreman; "" when unreported
	Judge     string  `json:"judge"`      // pass | fail | withdrawn | unknown
	Merge     string  `json:"merge"`      // merged | conflict | preserved | pending
	State     string  `json:"state"`      // running | done | abandoned (S12 §10.3)
	CostUSD   float64 `json:"cost_usd"`   // attribution ONLY (W4) — never summed into ticks.cost_usd
	TokensIn  int64   `json:"tokens_in"`
	TokensOut int64   `json:"tokens_out"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// TickWorker judge vocabulary (S12 §9.1 CHECK constraint).
const (
	TickWorkerJudgePass      = "pass"
	TickWorkerJudgeFail      = "fail"
	TickWorkerJudgeWithdrawn = "withdrawn"
	TickWorkerJudgeUnknown   = "unknown"
)

// TickWorker merge vocabulary (S12 §9.1 CHECK constraint).
const (
	TickWorkerMergeMerged    = "merged"
	TickWorkerMergeConflict  = "conflict"
	TickWorkerMergePreserved = "preserved"
	TickWorkerMergePending   = "pending"
)

// TickWorker state vocabulary (S12 §9.1 CHECK constraint / §10.3).
const (
	TickWorkerStateRunning   = "running"
	TickWorkerStateDone      = "done"
	TickWorkerStateAbandoned = "abandoned"
)

// Validate checks the enum fields against the vocabularies the tick_workers
// CHECK constraints enforce, returning a field-named error instead of
// letting the raw constraint fire at INSERT time. The DB CHECKs remain the
// authority; this is the caller-friendly pre-flight (S12 §13).
func (w *TickWorker) Validate() error {
	switch w.Judge {
	case TickWorkerJudgePass, TickWorkerJudgeFail, TickWorkerJudgeWithdrawn, TickWorkerJudgeUnknown:
	default:
		return fmt.Errorf("tick worker %q: invalid judge %q (want pass|fail|withdrawn|unknown)", w.TaskID, w.Judge)
	}
	switch w.Merge {
	case TickWorkerMergeMerged, TickWorkerMergeConflict, TickWorkerMergePreserved, TickWorkerMergePending:
	default:
		return fmt.Errorf("tick worker %q: invalid merge %q (want merged|conflict|preserved|pending)", w.TaskID, w.Merge)
	}
	switch w.State {
	case TickWorkerStateRunning, TickWorkerStateDone, TickWorkerStateAbandoned:
	default:
		return fmt.Errorf("tick worker %q: invalid state %q (want running|done|abandoned)", w.TaskID, w.State)
	}
	return nil
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
	// Demand (SCHED-GAP-1582): the enabled-weight the namespace carried
	// into the pack; Overcommitted the surplus HELD when demand exceeded
	// the cycle's allocation. 0 = not oversubscribed / not packed.
	Demand        int    `json:"demand"`
	Overcommitted int    `json:"overcommitted"`
	CreatedAt     string `json:"created_at"` // RFC3339
}
