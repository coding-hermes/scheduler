# S01 — System Architecture

**Status:** Draft  
**Depends on:** None  
**Pages target:** 3-4

---

## 1. Overview

The coding-hermes scheduler is a single Go binary (`schedulerd`) that replaces 33 static Hermes cron jobs with one weight-budget priority scheduler. It runs as a systemd-managed daemon on the same host as Hermes, binds `127.0.0.1:9090` (or a Unix socket at `/run/coding-hermes/scheduler.sock` with `0600` permissions), and exposes three interfaces:

- **HTTP REST API** at `/api/v1/` — project management, fleet status, tick history, health checks
- **MCP server** at `/mcp` — Hermes connects as an MCP client, auto-discovers fleet tools
- **Dashboard** at `/` — single-file HTML regenerated after each evaluation cycle

One trigger cron (60s, `no_agent=true`) replaces 33 foreman crons. The trigger hits `POST /api/v1/fleet/evaluate`. The scheduler decides which projects to tick, spawns foremen via `hermes chat --quiet`, captures session IDs, and tracks outcomes.

### Architecture Diagram

```mermaid
graph TB
    subgraph External
        HC[Hermes Cron<br/>trigger every 60s]
        HM[Hermes MCP Client<br/>fleet tools]
        BR[Bane's Browser<br/>dashboard]
        WS[Watchdog Scripts<br/>health checks]
    end

    subgraph "schedulerd (Go binary, systemd)"
        API[HTTP REST API<br/>:9090/api/v1]
        MCP[MCP Server<br/>:9090/mcp]
        DASH[Dashboard<br/>:9090/]

        subgraph "Scheduler Core"
            NS[Namespace Allocator<br/>two-axis budget distribution]
            URG[Urgency Calculator]
            PK[Multi-Pool Packer<br/>flat or namespaced]
            BE[Borrowing Engine<br/>idle capacity redistribution]
            SL[SlotPool<br/>concurrent spawn semaphore]
            SP[Spawner]
            LC[Tick Lifecycle Tracker]
        end

        DB[(SQLite<br/>scheduler.db)]
        SYNC[DuckBrain Sync<br/>every 5 min]
    end

    subgraph "DuckBrain"
        DF[/fleet/projects/]
        DN[/fleet/namespaces/]
        DS[/fleet/summary]
        DE[/fleet/events]
    end

    HC -->|POST /evaluate| API
    HM -->|MCP tools| MCP
    BR -->|HTTP GET| DASH
    WS -->|GET /health| API

    API --> DB
    MCP --> API
    DASH --> DB

    NS --> URG
    URG --> PK
    BE --> PK
    PK --> SL
    SL --> SP
    SP --> LC
    LC --> DB
    SYNC --> DB
    SYNC --> DF
    SYNC --> DN
    SYNC --> DS
    SYNC --> DE

    SP -->|hermes chat --quiet| HC
```

---

## 2. Dependencies

| Dependency | Version | Purpose | Failure Mode |
|-----------|---------|---------|-------------|
| `hermes-agent` CLI | Any with `--quiet` flag | Spawning foreman ticks | Scheduler cannot spawn foremen; marks ticks as `failed` |
| DuckBrain MCP | Latest | Read-replica sync | Sync silently fails; dashboard shows stale data; foremen lose cross-tick context |
| SQLite (mattn/go-sqlite3) | 3.x via CGo | Operational store | Scheduler cannot start; `systemd` restarts it |
| Go stdlib `net/http` | 1.22+ | HTTP server | No API/MCP/Dashboard |
| systemd | Any | Process supervision | Manual start required |

All dependencies are local — no network calls to external services except `hermes chat` (local subprocess) and DuckBrain MCP (localhost).

---

## 3. Interfaces

### 3.1 System Boundary Interfaces

```go
// Scheduler is the top-level orchestrator.
type Scheduler struct {
    db        *database.Store
    nsAlloc   *NamespaceAllocator
    urgency   *UrgencyCalculator
    packer    *WeightPacker        // flat mode (default)
    multiPool *MultiPoolPacker     // namespace mode
    borrowing *BorrowingEngine
    slotPool  *SlotPool            // concurrent spawn semaphore
    spawner   *Spawner
    lifecycle *LifecycleTracker
    syncer    *DuckBrainSyncer
    dashboard *DashboardGenerator
}

func NewScheduler(cfg Config) (*Scheduler, error)
func (s *Scheduler) Run(ctx context.Context) error       // blocking — runs evaluation loop
func (s *Scheduler) Evaluate(ctx context.Context) (*EvaluationResult, error)  // one-off, called by API
func (s *Scheduler) Shutdown(ctx context.Context) error   // graceful — completes running ticks
```

### 3.2 Database Interface

```go
type Store struct {
    db *sql.DB
}

func NewStore(path string) (*Store, error)
func (s *Store) Migrate(ctx context.Context) error          // auto-run schema migrations
func (s *Store) Close() error

// Projects
func (s *Store) ListProjects(ctx context.Context, enabledOnly bool) ([]Project, error)
func (s *Store) GetProject(ctx context.Context, name string) (*Project, error)
func (s *Store) CreateProject(ctx context.Context, p Project) error
func (s *Store) UpdateProject(ctx context.Context, name string, patch ProjectPatch) error
func (s *Store) DeleteProject(ctx context.Context, name string) error  // soft-delete (enabled=0)

// Ticks
func (s *Store) CreateTick(ctx context.Context, t Tick) error
func (s *Store) UpdateTickOutcome(ctx context.Context, tickID string, o TickOutcome) error
func (s *Store) GetTick(ctx context.Context, tickID string) (*Tick, error)
func (s *Store) ListTicks(ctx context.Context, project string, limit int) ([]Tick, error)
func (s *Store) PruneOldTicks(ctx context.Context, project string, keep int) (int, error)

// Events
func (s *Store) AppendEvent(ctx context.Context, e Event) error
func (s *Store) ListEvents(ctx context.Context, filter EventFilter) ([]Event, error)
```

### 3.3 Scheduler Sub-Component Interfaces

```go
type UrgencyCalculator struct {
    minInterval time.Duration
    maxInterval time.Duration
    numLevels   int
}

func NewUrgencyCalculator(minI, maxI time.Duration, levels int) *UrgencyCalculator
// ComputeInterval returns the geometric interval for a given priority.
func (u *UrgencyCalculator) ComputeInterval(priority float64) time.Duration
// ComputeUrgency returns the urgency value for a project.
func (u *UrgencyCalculator) ComputeUrgency(p Project, now time.Time, lastCompleted *time.Time) float64
// SetRange updates min/max interval at runtime, recalculates all intervals.
func (u *UrgencyCalculator) SetRange(minI, maxI time.Duration)


type WeightPacker struct {
    budget         int
    maxConcurrent  int
}

func NewWeightPacker(budget, maxConcurrent int) *WeightPacker
// Pack sorts projects by urgency, greedily packs into weight budget.
// Returns the subset that should run this tick.
func (w *WeightPacker) Pack(projects []ProjectWithUrgency, running []string) []Project
func (w *WeightPacker) SetBudget(budget int)


type Spawner struct {
    db            *sql.DB
    maxConcurrent int
    timeout       time.Duration
}

func NewSpawner(db *sql.DB, maxConcurrent int, timeout ...time.Duration) *Spawner
// Spawn launches a foreman process and returns a spawned tick handle.
func (s *Spawner) Spawn(project PackedProject, tickID string) (*SpawnedTick, error)
// SpawnMethodCounts returns (http, exec) counts for telemetry.
func (s *Spawner) SpawnMethodCounts() (httpCount, execCount int64)


type SlotPool struct {
    sem       chan string     // buffered channel = semaphore, value = project name
    maxSlots  int
    timeout   time.Duration
    spawner   *Spawner
    lifecycle *LifecycleTracker
    freedCh   chan struct{}   // fires when a slot is released
}

func NewSlotPool(maxConcurrent int, timeout time.Duration, spawner *Spawner, lifecycle *LifecycleTracker) *SlotPool
// Available returns the number of free slots.
func (p *SlotPool) Available() int
// Running returns the count of currently occupied slots.
func (p *SlotPool) Running() int
// RunningSet returns the set of project names currently in slots.
// Used by the packer to prevent duplicate spawns.
func (p *SlotPool) RunningSet() map[string]bool
// Spawn acquires a slot and launches the tick asynchronously.
// Returns immediately after queuing — does not block on spawn completion.
func (p *SlotPool) Spawn(project PackedProject, now time.Time, noDeliver bool, db *sql.DB)
// SlotFreed returns a channel that fires when any slot is released.
func (p *SlotPool) SlotFreed() <-chan struct{}
// ReleaseAll drains all occupied slots (used during shutdown).
func (p *SlotPool) ReleaseAll()


type LifecycleTracker struct {
    store *Store
}

func NewLifecycleTracker(store *Store) *LifecycleTracker
// CompleteTick queries session outcome and updates the tick record.
func (l *LifecycleTracker) CompleteTick(ctx context.Context, rt *RunningTick) error
```

### 3.4 Config Struct

```go
type Config struct {
    Port          int           `env:"SCHEDULER_PORT"          default:"9090"`
    Socket        string        `env:"SCHEDULER_SOCKET"        default:""`  // overrides Port if set
    DBPath        string        `env:"SCHEDULER_DB_PATH"       default:"~/.hermes/coding-hermes/scheduler.db"`
    Budget        int           `env:"SCHEDULER_BUDGET"        default:"100"`
    MinInterval   time.Duration `env:"SCHEDULER_MIN_INTERVAL"  default:"20m"`
    MaxInterval   time.Duration `env:"SCHEDULER_MAX_INTERVAL"  default:"24h"`
    NumLevels     int           `env:"SCHEDULER_NUM_LEVELS"    default:"10"`
    MaxConcurrent int           `env:"SCHEDULER_MAX_CONCURRENT" default:"8"`
    SpawnTimeout  time.Duration `env:"SCHEDULER_SPAWN_TIMEOUT" default:"30m"`
    LoopInterval  time.Duration `env:"SCHEDULER_LOOP_INTERVAL" default:"60s"`
    SyncInterval  time.Duration `env:"SCHEDULER_SYNC_INTERVAL" default:"5m"`
    NamespaceMode bool          `env:"SCHEDULER_NAMESPACE_MODE" default:"false"`
}

func (c Config) Validate() error {
    // Port must be 1-65535 or Socket must be non-empty
    // Budget must be > 0
    // MinInterval < MaxInterval
    // NumLevels >= 2
    // MaxConcurrent > 0
    // SpawnTimeout > LoopInterval
    // SyncInterval > 0
    return nil
}
```

---

## 4. Behavior

### 4.1 Evaluation Loop (every `LoopInterval`)

```
1. Load all enabled projects from SQLite
2. For each project:
   a. Compute interval = geometric_interval(priority, min, max, levels)
   b. Compute urgency = priority * (1 + elapsed / interval) ^ decay_rate
3. Sort projects by urgency descending
4. Greedy pack into weight budget:
   a. budget_remaining = budget
   b. For each project in urgency order:
      - Skip if in SlotPool.RunningSet() (already running)
      - Skip if cooldown not elapsed
      - Skip if weight > budget_remaining
      - Add to run queue, subtract weight
5. For each project in run queue, dispatch to SlotPool:
   a. SlotPool.Spawn(project, now, noDeliver, db) — async, returns immediately
   b. SlotPool acquires a semaphore slot, spawns via Spawner, tracks lifecycle
6. On SlotPool.SlotFreed() signal, re-evaluate packer for pending projects
7. For each completed tick (via LifecycleTracker):
   a. Query session outcome (hermes sessions export --dry-run)
   b. Update tick record (status=completed, outcome, commits, cost)
8. Generate dashboard HTML
9. Sync to DuckBrain
10. Prune old ticks (>200 per project)
```

### 4.2 Evaluation Result (returned by API)

```go
type EvaluationResult struct {
    EvaluatedAt  time.Time         `json:"evaluated_at"`
    BudgetUsed   int               `json:"budget_used"`
    BudgetTotal  int               `json:"budget_total"`
    Spawned      []SpawnedProject  `json:"spawned"`
    Skipped      []SkippedProject  `json:"skipped"`
    Errors       []string          `json:"errors"`
}

type SpawnedProject struct {
    Name     string  `json:"name"`
    Weight   int     `json:"weight"`
    Priority int `json:"priority"`
    Urgency  float64 `json:"urgency"`
    TickID   string  `json:"tick_id"`
}

type SkippedProject struct {
    Name   string `json:"name"`
    Reason string `json:"reason"`  // "cooldown_not_elapsed" | "budget_exhausted" | "max_concurrent" | "disabled"
}
```

---

## 5. Data

See **S02 — Data Model** for complete DDL, Go structs, and DuckBrain key schemas.

---

## 6. States

### 6.1 Scheduler Process States

```
INIT → RUNNING → SHUTTING_DOWN → STOPPED
           ↘ CRASHED → (systemd restarts) → INIT
```

### 6.2 Tick States (per spawned foreman)

```
QUEUED → RUNNING → COMPLETED
               ↘ FAILED
               ↘ TIMED_OUT
```

---

## 7. Errors

| Component | Error | HTTP Status | MCP Error | Recovery |
|-----------|-------|-------------|-----------|----------|
| Database | `sqlite3: database is locked` | 503 | Internal error | Retry with backoff; WAL mode minimizes this |
| Database | `UNIQUE constraint failed: projects.name` | 409 | Invalid params | Return conflict; client must rename |
| SpawnEngine | `hermes: command not found` | 500 | Internal error | Mark all pending ticks as failed; alert |
| SpawnEngine | Process killed (SIGKILL) | — | — | Mark tick as `failed`; do not retry |
| SpawnEngine | Timeout (30 min) | — | — | Mark tick as `timeout`; kill process |
| UrgencyCalc | `MinInterval >= MaxInterval` | 400 | Invalid params | Return validation error |
| Packer | `Budget <= 0` | 400 | Invalid params | Return validation error |
| API | Invalid JSON body | 400 | Invalid params | Return error with field name |
| API | Project not found (GET/PUT/DELETE) | 404 | Not found | Return error with project name |
| Sync | DuckBrain MCP unreachable | — | — | Log warning; retry next cycle; dashboard shows stale timestamp |
| All | Panic | 500 | Internal error | `recover()` in HTTP middleware; log stack trace; systemd restarts |

---

## 8. Testing

See **S10 — Testing Strategy** for full test scenarios.

Minimum bar before any PR merges:
- Unit tests: urgency calculator, weight packer (pure logic, no I/O)
- Integration tests: SQLite CRUD, spawn engine with mock process
- Contract tests: REST API request/response shapes, MCP tool schemas
- End-to-end: start scheduler → POST evaluate → verify tick spawned → verify outcome tracked

---

## 9. Security

| Vector | Mitigation |
|--------|-----------|
| Remote access to API | Bind `127.0.0.1` only; or Unix socket with `0600` permissions |
| SQL injection | Use parameterized queries exclusively (Go `database/sql` with `?` placeholders) |
| Shell injection in spawn | Build command with `os/exec.Command` (no shell interpolation); args as `[]string` |
| Secret exposure in logs | Redact `Authorization` headers; never log full API responses containing keys |
| Malicious project config | Validate all input: weight 1-100, priority 0.5-10, cooldown >= 0, decay >= 0 |
| DuckBrain write access | Use read-only MCP client if available; otherwise accept write risk (localhost only) |

---

## 10. Performance

| Metric | Target | Measurement |
|--------|--------|-------------|
| Evaluation cycle | < 100ms (excluding spawns) | Timer around `Evaluate()` |
| Spawn latency | < 500ms from evaluation to process start | Timer in SpawnEngine |
| API response (read) | < 10ms p50, < 50ms p99 | HTTP middleware timer |
| API response (write) | < 20ms p50, < 100ms p99 | HTTP middleware timer |
| Dashboard render | < 50ms | Timer in DashboardGenerator |
| SQLite query (any) | < 5ms | WAL mode, indexed queries |
| Memory (steady state) | < 50 MB | Go runtime metrics |
| 33 projects × 8 concurrent | < 8 spawned processes at any time | Enforced by maxConcurrent |

### Directory Tree

```
coding-herms-scheduler/
├── cmd/
│   └── schedulerd/
│       ├── main.go          # entry point: parse flags, init DB, wire, run
│       └── config.go        # Config struct, env parsing, validation
├── internal/
│   ├── scheduler/
│   │   ├── urgency.go       # urgency calculator
│   │   ├── urgency_test.go
│   │   ├── packer.go        # weight-budget packer
│   │   ├── packer_test.go
│   │   ├── spawn.go         # spawn engine
│   │   ├── slot_pool.go     # concurrent slot semaphore (SlotPool)
│   │   ├── slot_pool_test.go
│   │   ├── lifecycle.go     # tick lifecycle tracker
│   │   └── loop.go          # main evaluation loop
│   ├── database/
│   │   ├── schema.go        # DDL constants
│   │   ├── migrations.go    # versioned migration runner
│   │   ├── store.go         # Store struct, NewStore, Close
│   │   ├── projects.go      # project CRUD
│   │   ├── ticks.go         # tick CRUD
│   │   ├── events.go        # event append + query
│   │   └── database_test.go # integration tests with in-memory SQLite
│   ├── api/
│   │   ├── server.go        # HTTP mux, middleware, route registration
│   │   ├── handlers.go      # handler functions
│   │   ├── middleware.go    # logging, recovery, CORS
│   │   └── api_test.go      # httptest-based handler tests
│   ├── mcp/
│   │   ├── server.go        # MCP streamable-http handler
│   │   ├── tools.go         # tool registration and dispatch
│   │   └── mcp_test.go      # MCP protocol compliance tests
│   ├── dashboard/
│   │   ├── generator.go     # Go html/template with embedded CSS
│   │   ├── templates/       # template files (embedded)
│   │   └── dashboard_test.go
│   └── sync/
│       ├── duckbrain.go     # DuckBrain read-replica sync
│       └── sync_test.go
├── deploy/
│   └── coding-hermes-scheduler.service
├── specs/
│   ├── S01-system-architecture.md
│   ├── S02-data-model.md
│   ├── S03-urgency-calculator.md
│   ├── S04-weight-packer.md
│   ├── S05-spawn-engine-lifecycle.md
│   ├── S06-rest-api.md
│   ├── S07-multi-namespace-extension.md
│   ├── S08-dashboard.md
│   ├── S09-hermes-plugin.md
│   ├── S10-testing-strategy.md
│   └── S11-deployment-migration.md
├── .coding-hermes/
│   └── tasks.md
├── Makefile
├── README.md
├── go.mod
└── go.sum
```
