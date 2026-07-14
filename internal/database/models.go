package database

// Project is a single managed codebase the scheduler may spawn ticks against.
// Field ordering matches the projects table column order for scan ergonomics.
type Project struct {
	Name      string  // PRIMARY KEY — also the DuckBrain project key
	RepoURL   string  // git clone URL
	Workdir   string  // absolute path to the working copy on this host
	Weight    int     // 1..100 — weight budget consumed per tick (default 10)
	Priority  int     // 1..10 — base urgency multiplier (default 5)
	CooldownS int     // seconds between successive ticks (default 900)
	DecayRate float64 // urgency decay rate (default 1.0)
	Model     string  // LLM model id passed to the spawned agent
	Provider  string  // LLM provider id passed to the spawned agent
	Command   string  // optional: custom spawn command (overrides default hermes chat)
	Enabled   bool    // disabled projects are never scheduled
	CreatedAt string  // RFC3339 timestamp
	UpdatedAt string  // RFC3339 timestamp
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

// EventLevel enumerates the severity tiers for event log entries.
type EventLevel string

const (
	LevelInfo     EventLevel = "info"
	LevelWarn     EventLevel = "warn"
	LevelError    EventLevel = "error"
	LevelDecision EventLevel = "decision"
)

// Event is a single log line in the operational event log. Decisions and
// errors land here; info/warn capture routine operational notes.
type Event struct {
	ID          int64  // AUTOINCREMENT PK
	Timestamp   string // RFC3339 — when the event occurred (not insertion time)
	Level       EventLevel
	ProjectName string // optional — empty for fleet-wide events
	Message     string
	Detail      string // free-form context, often JSON
	CreatedAt   string // RFC3339 — when the row was inserted
}
