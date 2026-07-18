package database

// Project is a single managed codebase the scheduler may spawn ticks against.
// Field ordering matches the projects table column order for scan ergonomics.
type Project struct {
	Name        string  // PRIMARY KEY — also the DuckBrain project key
	RepoURL     string  // git clone URL
	Workdir     string  // absolute path to the working copy on this host
	Weight      int     // 1..100 — weight budget consumed per tick (default 10)
	Priority    int     // 1..10 — base urgency multiplier (default 5)
	CooldownS   int     // seconds between successive ticks (default 900)
	DecayRate   float64 // urgency decay rate (default 1.0)
	Model       string  // LLM model id passed to the spawned agent
	Provider    string  // LLM provider id passed to the spawned agent
	Command     string  // optional: custom spawn command (overrides default hermes chat)
	NamespaceID *string // optional: FK → namespaces.id; NULL = unscheduled in namespace mode
	Deliver     string  // delivery target: platform:chat_id:thread_id (e.g. telegram:-1003310984808:12)
	Enabled     bool    // disabled projects are never scheduled
	CreatedAt   string  // RFC3339 timestamp
	UpdatedAt   string  // RFC3339 timestamp
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
	ID           string // PRIMARY KEY — see NextTickID for format
	ProjectName  string // FK → projects.name
	SessionID    string // captured from spawned process stdout
	Status       TickStatus
	Outcome      TickOutcome // set on terminal transition
	SpawnedAt    string      // RFC3339 — when the process started
	CompletedAt  string      // RFC3339 — when the process ended
	ExitCode     int
	Commits      int
	FilesChanged int
	TokensIn     int64
	TokensOut    int64
	CostUSD      float64
	Urgency      float64 // urgency score at spawn time
	WeightUsed   int
	Error        string
	CreatedAt    string
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
	ID        int64         // AUTOINCREMENT PK
	Severity  EventSeverity // CRITICAL, HIGH, MEDIUM, LOW, INFO
	Component string        // system component that emitted the event
	Message   string
	Details   string // free-form context, often JSON
	CreatedAt string // RFC3339 — when the row was inserted
}

// Namespace represents a weight pool for related cron jobs.
// Each namespace gets a share of the global budget (B=100) via a two-phase
// allocation algorithm: reserved floor + proportional remainder, capped by hard_cap.
type Namespace struct {
	ID          string `json:"id"`          // PRIMARY KEY — unique slug (e.g. "coding-hermes")
	Weight      int    `json:"weight"`      // 1..100 — relative weight for proportional allocation
	Reserved    int    `json:"reserved"`    // >= 0 — guaranteed floor budget units
	HardCap     int    `json:"hard_cap"`    // >= 0 — maximum budget; 0 means no cap (interpret as B)
	Enabled     bool   `json:"enabled"`     // disabled namespaces get zero allocation
	Description string `json:"description"` // human-readable label
	CreatedAt   string `json:"created_at"`  // RFC3339
	UpdatedAt   string `json:"updated_at"`  // RFC3339
}

// NamespacePatch is used for partial updates. Only non-nil fields are applied.
type NamespacePatch struct {
	Weight      *int    `json:"weight,omitempty"`
	Reserved    *int    `json:"reserved,omitempty"`
	HardCap     *int    `json:"hard_cap,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	Description *string `json:"description,omitempty"`
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
