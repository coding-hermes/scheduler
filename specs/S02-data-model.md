# S02 — Data Model

**Status:** Draft  
**Depends on:** S01  

---

## 1. Overview

Two storage systems with distinct roles:

| System | Role | Authoritative? | Access Pattern |
|--------|------|---------------|----------------|
| SQLite (`scheduler.db`) | Operational store | **Yes** | WAL mode, read/write every tick |
| DuckBrain (`coding-hermes` namespace) | Read replica | No | Overwrite every 5 min, cross-session queries |

SQLite is the source of truth. DuckBrain is a compact snapshot for foremen and Bane's other sessions to read fleet state without touching SQLite.

---

## 2. Dependencies

| Dependency | Version | Purpose |
|-----------|---------|---------|
| `github.com/mattn/go-sqlite3` | Latest | SQLite driver (CGo) |
| `database/sql` | stdlib | Database interface |
| DuckBrain MCP | Latest | Read-replica sync via `remember` tool |

---

## 3. SQLite DDL

### 3.0 Namespaces Table (Migration v2)

```sql
CREATE TABLE IF NOT EXISTS namespaces (
    id          TEXT PRIMARY KEY NOT NULL,
    weight      INTEGER NOT NULL DEFAULT 10 CHECK(weight >= 1 AND weight <= 100),
    reserved    INTEGER NOT NULL DEFAULT 1 CHECK(reserved >= 0),
    hard_cap    INTEGER NOT NULL DEFAULT 100 CHECK(hard_cap >= 0),
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
    description TEXT,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
```

### 3.1 Projects Table

```sql
CREATE TABLE IF NOT EXISTS projects (
    name         TEXT PRIMARY KEY NOT NULL,
    repo_url     TEXT NOT NULL,
    workdir      TEXT NOT NULL,
    weight       INTEGER NOT NULL DEFAULT 10 CHECK(weight >= 1 AND weight <= 100),
    priority     REAL NOT NULL DEFAULT 5.0 CHECK(priority >= 0.5 AND priority <= 10.0),
    cooldown_s   INTEGER NOT NULL DEFAULT 300 CHECK(cooldown_s >= 0),
    decay_rate   REAL NOT NULL DEFAULT 1.0 CHECK(decay_rate >= 0),
    enabled      INTEGER NOT NULL DEFAULT 1 CHECK(enabled IN (0, 1)),
    namespace_id TEXT REFERENCES namespaces(id) ON DELETE SET NULL,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_projects_namespace ON projects(namespace_id);
```

**Migration v2:** `ALTER TABLE projects ADD COLUMN namespace_id TEXT REFERENCES namespaces(id) ON DELETE SET NULL`. Existing projects get `NULL` (unscheduled in namespace mode; backward-compatible — flat mode ignores the column).

### 3.2 Ticks Table

```sql
CREATE TABLE IF NOT EXISTS ticks (
    id          TEXT PRIMARY KEY NOT NULL,
    project     TEXT NOT NULL REFERENCES projects(name) ON DELETE CASCADE,
    session_id  TEXT,
    status      TEXT NOT NULL DEFAULT 'queued'
                    CHECK(status IN ('queued', 'running', 'completed', 'failed', 'timeout')),
    outcome     TEXT CHECK(outcome IN ('committed', 'dry_run', 'failed', 'timeout', NULL)),
    spawned_at  TEXT,
    completed_at TEXT,
    exit_code   INTEGER,
    commits     INTEGER NOT NULL DEFAULT 0,
    files_changed INTEGER NOT NULL DEFAULT 0,
    tokens_in   INTEGER,
    tokens_out  INTEGER,
    cost_usd    REAL,
    urgency     REAL,
    weight_used INTEGER,
    error       TEXT
);

CREATE INDEX IF NOT EXISTS idx_ticks_project ON ticks(project, spawned_at DESC);
CREATE INDEX IF NOT EXISTS idx_ticks_status ON ticks(status);
```

**Tick ID format:** `<project>-<YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>` — ensures sortability and uniqueness.

Example: `muster-2026-07-12-14-03-01`

### 3.3 Namespace Ticks Table (Migration v2)

Records per-namespace utilization for each evaluation cycle. Used for borrowing decisions and dashboard visibility.

```sql
CREATE TABLE IF NOT EXISTS namespace_ticks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    tick_group   TEXT NOT NULL,           -- group key: <YYYY>-<MM>-<DD>-<HH>-<mm>-<ss>
    namespace_id TEXT NOT NULL REFERENCES namespaces(id),
    allocated    INTEGER NOT NULL,        -- budget given this tick
    used         INTEGER NOT NULL,        -- budget actually consumed (sum of effective weights)
    borrowed     INTEGER NOT NULL DEFAULT 0, -- extra budget from other namespaces
    lent         INTEGER NOT NULL DEFAULT 0, -- budget given to other namespaces
    job_count    INTEGER NOT NULL,        -- how many jobs ran
    created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_namespace_ticks_group ON namespace_ticks(tick_group);
CREATE INDEX IF NOT EXISTS idx_namespace_ticks_ns ON namespace_ticks(namespace_id, created_at DESC);
```

### 3.4 Events Table

```sql
-- v5: recreated with severity/component/details columns
CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    severity   TEXT NOT NULL CHECK(severity IN ('CRITICAL','HIGH','MEDIUM','LOW','INFO')),
    component  TEXT NOT NULL DEFAULT '',
    message    TEXT NOT NULL,
    details    TEXT DEFAULT '{}',
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_severity ON events(severity, created_at DESC);
```

### 3.5 Schema Migrations Table

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  TEXT NOT NULL DEFAULT (datetime('now')),
    description TEXT NOT NULL
);
```

Migration versions:
- **v1**: Initial schema — `projects`, `ticks`, `events`, `schema_migrations`
- **v2**: Multi-namespace — `namespaces`, `namespace_ticks`, `projects.namespace_id`, indexes
- **v5**: Events table rebuilt with `severity`/`component`/`details` columns (CRITICAL/HIGH/MEDIUM/LOW/INFO)

### 3.6 WAL Mode and Foreign Keys

```go
// Applied on every Store initialization, after opening the DB:
db.Exec("PRAGMA journal_mode=WAL")
db.Exec("PRAGMA foreign_keys=ON")
db.Exec("PRAGMA busy_timeout=5000")  // 5 seconds
```

---

## 4. Go Model Structs

```go
package database

import "time"

// Project represents a managed coding-hermes project.
type Project struct {
    Name        string    `json:"name"`
    RepoURL     string    `json:"repo_url"`
    Workdir     string    `json:"workdir"`
    Weight      int       `json:"weight"`
    Priority    int       `json:"priority"`
    CooldownS   int       `json:"cooldown_s"`
    DecayRate   float64   `json:"decay_rate"`
    Enabled     bool      `json:"enabled"`
    NamespaceID *string   `json:"namespace_id"` // NULL = unscheduled in namespace mode
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// ProjectPatch is used for partial updates.
type ProjectPatch struct {
    Weight     *int     `json:"weight,omitempty"`
    Priority   *int `json:"priority,omitempty"`
    CooldownS  *int     `json:"cooldown_s,omitempty"`
    DecayRate  *float64 `json:"decay_rate,omitempty"`
    Enabled    *bool    `json:"enabled,omitempty"`
}

// Tick represents one foreman spawn.
type Tick struct {
    ID           string     `json:"id"`
    ProjectName  string     `json:"project_name"`
    SessionID    *string    `json:"session_id"`
    Status       string     `json:"status"`     // queued|running|completed|failed|timeout
    Outcome      *string    `json:"outcome"`     // committed|dry_run|failed|timeout
    SpawnedAt    *time.Time `json:"spawned_at"`
    CompletedAt  *time.Time `json:"completed_at"`
    ExitCode     *int       `json:"exit_code"`
    Commits      int        `json:"commits"`
    FilesChanged int        `json:"files_changed"`
    TokensIn     *int       `json:"tokens_in"`
    TokensOut    *int       `json:"tokens_out"`
    CostUSD      *float64   `json:"cost_usd"`
    Urgency      *float64   `json:"urgency"`
    WeightUsed   *int       `json:"weight_used"`
    Error        *string    `json:"error"`
    CreatedAt    string     // RFC3339
}

// TickOutcome is written after a tick completes.
type TickOutcome struct {
    Status       string   // completed|failed|timeout
    Outcome      string   // committed|dry_run|failed|timeout
    ExitCode     int
    Commits      int
    FilesChanged int
    TokensIn     int
    TokensOut    int
    CostUSD      float64
    Error        string
}

// Event represents an audit log entry. Uses severity-based classification
// (CRITICAL/HIGH/MEDIUM/LOW/INFO) and component attribution.
type Event struct {
    ID        int64         `json:"id"`
    Severity  EventSeverity `json:"severity"`  // CRITICAL, HIGH, MEDIUM, LOW, INFO
    Component string        `json:"component"` // system component that emitted the event
    Message   string        `json:"message"`
    Details   string        `json:"details"`   // JSON string
    CreatedAt string        `json:"created_at"` // RFC3339
}

// EventSeverity is the severity tier for event log entries.
type EventSeverity string

const (
    SeverityCritical EventSeverity = "CRITICAL"
    SeverityHigh     EventSeverity = "HIGH"
    SeverityMedium   EventSeverity = "MEDIUM"
    SeverityLow      EventSeverity = "LOW"
    SeverityInfo     EventSeverity = "INFO"
)

// EventFilter for querying events.
type EventFilter struct {
    Severity  string // empty = all
    Component string // empty = all
    Limit     int    // default 50, max 500
    Offset    int
}

// Namespace represents a weight pool for related cron jobs (Migration v2).
type Namespace struct {
    ID          string    `json:"id"`
    Weight      int       `json:"weight"`
    Reserved    int       `json:"reserved"`
    HardCap     int       `json:"hard_cap"`
    Enabled     bool      `json:"enabled"`
    Description string    `json:"description"`
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

// NamespacePatch is used for partial updates to a namespace.
type NamespacePatch struct {
    Weight      *int    `json:"weight,omitempty"`
    Reserved    *int    `json:"reserved,omitempty"`
    HardCap     *int    `json:"hard_cap,omitempty"`
    Enabled     *bool   `json:"enabled,omitempty"`
    Description *string `json:"description,omitempty"`
}

// NamespaceTick records per-namespace utilization per evaluation cycle.
type NamespaceTick struct {
    ID          int64     `json:"id"`
    TickGroup   string    `json:"tick_group"`
    NamespaceID string    `json:"namespace_id"`
    Allocated   int       `json:"allocated"`
    Used        int       `json:"used"`
    Borrowed    int       `json:"borrowed"`
    Lent        int       `json:"lent"`
    JobCount    int       `json:"job_count"`
    CreatedAt   time.Time `json:"created_at"`
}
```

---

## 5. Query Patterns

### 5.1 List all enabled projects (called every tick)

```sql
SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
       enabled, created_at, updated_at
FROM projects
WHERE enabled = 1
ORDER BY name;
```

### 5.2 Insert a tick (atomic, uses tick ID as PK)

```sql
INSERT INTO ticks (id, project, status, urgency, weight_used, spawned_at)
VALUES (?, ?, 'running', ?, ?, datetime('now'));
```

### 5.3 Update tick outcome after completion

```sql
UPDATE ticks
SET status = ?, outcome = ?, completed_at = datetime('now'),
    exit_code = ?, commits = ?, files_changed = ?,
    tokens_in = ?, tokens_out = ?, cost_usd = ?, error = ?
WHERE id = ?;
```

### 5.4 Get recent ticks for a project (dashboard, per-project view)

```sql
SELECT id, session_id, status, outcome, spawned_at, completed_at,
       exit_code, commits, cost_usd
FROM ticks
WHERE project = ?
ORDER BY spawned_at DESC
LIMIT ?;  -- typically 20
```

### 5.5 Count running ticks (concurrency check)

```sql
SELECT COUNT(*) FROM ticks WHERE status = 'running';
```

### 5.6 Prune old ticks (keep last 200 per project)

```sql
DELETE FROM ticks
WHERE project = ? AND id NOT IN (
    SELECT id FROM ticks WHERE project = ? ORDER BY spawned_at DESC LIMIT 200
);
```

### 5.7 Get last completed tick for a project (cooldown check)

```sql
SELECT id, completed_at, outcome
FROM ticks
WHERE project = ? AND status = 'completed'
ORDER BY completed_at DESC
LIMIT 1;
```

### 5.8 List all enabled namespaces (called every tick in namespace mode)

```sql
SELECT id, weight, reserved, hard_cap, enabled, description, created_at, updated_at
FROM namespaces
WHERE enabled = 1
ORDER BY weight DESC;
```

### 5.9 Get projects by namespace (intra-namespace packing)

```sql
SELECT name, repo_url, workdir, weight, priority, cooldown_s, decay_rate,
       enabled, namespace_id, created_at, updated_at
FROM projects
WHERE enabled = 1 AND namespace_id = ?
ORDER BY name;
```

### 5.10 Insert namespace tick (after each evaluation cycle)

```sql
INSERT INTO namespace_ticks (tick_group, namespace_id, allocated, used, borrowed, lent, job_count)
VALUES (?, ?, ?, ?, ?, ?, ?);
```

### 5.11 Get namespace utilization history (dashboard)

```sql
SELECT tick_group, allocated, used, borrowed, lent, job_count, created_at
FROM namespace_ticks
WHERE namespace_id = ?
ORDER BY created_at DESC
LIMIT ?;  -- typically 20
```

### 5.12 Move project to namespace

```sql
UPDATE projects SET namespace_id = ?, updated_at = datetime('now') WHERE name = ?;
```

---

## 6. DuckBrain Key Schema

### 6.1 `/fleet/summary` — Fleet-Wide Dashboard Data

```json
{
    "updated_at": "2026-07-12T14:05:00Z",
    "total_projects": 33,
    "enabled_projects": 31,
    "budget_total": 100,
    "budget_used": 72,
    "active_ticks": 7,
    "ticks_today": 142,
    "projects": [
        {
            "name": "muster",
            "weight": 25,
            "priority": 8.0,
            "urgency": 12.4,
            "last_tick": "2026-07-12T14:03:01Z",
            "last_outcome": "committed",
            "session_id": "20260712_140301_a1b2c3d4"
        }
    ]
}
```

### 6.2 `/fleet/projects/<name>/status` — Per-Project Compact Status

```json
{
    "name": "muster",
    "weight": 25,
    "priority": 8.0,
    "cooldown_s": 300,
    "decay_rate": 1.0,
    "enabled": true,
    "current_interval": "20m",
    "last_tick": "2026-07-12T14:03:01Z",
    "last_outcome": "committed",
    "last_session_id": "20260712_140301_a1b2c3d4",
    "last_commits": 2,
    "last_cost": 0.12,
    "ticks_today": 8,
    "commits_today": 5,
    "cost_today": 0.94,
    "updated_at": "2026-07-12T14:05:00Z"
}
```

### 6.3 `/fleet/events` — Notable Events

```json
[
    {
        "id": 142,
        "severity": "INFO",
        "component": "spawn",
        "message": "Spawned foreman tick: urgency=12.4, weight=25",
        "details": "{\"tick_id\":\"muster-2026-07-12-14-03-01\"}",
        "created_at": "2026-07-12T14:03:00Z"
    },
    {
        "id": 141,
        "severity": "HIGH",
        "component": "sync",
        "message": "DuckBrain sync failed: connection refused (will retry in 5m)",
        "details": "{}",
        "created_at": "2026-07-12T14:02:00Z"
    }
]
```

DuckBrain keys are **overwritten** each sync cycle, not appended. The scheduler writes the full blob each time.

### 6.4 `/fleet/namespaces` — Namespace Registry

```json
{
    "updated_at": "2026-07-12T14:05:00Z",
    "mode": "multi-namespace",
    "namespaces": [
        {
            "id": "coding-hermes",
            "weight": 60,
            "reserved": 25,
            "hard_cap": 85,
            "enabled": true,
            "project_count": 31,
            "last_allocation": 61,
            "last_used": 58,
            "last_borrowed": 0,
            "last_lent": 3,
            "last_jobs": 6
        }
    ]
}
```

### 6.5 `/fleet/namespaces/<id>/status` — Per-Namespace Detail

```json
{
    "id": "coding-hermes",
    "weight": 60,
    "reserved": 25,
    "hard_cap": 85,
    "enabled": true,
    "description": "Coding Hermes foreman fleet",
    "project_count": 31,
    "current_allocation": 61,
    "current_usage": 58,
    "borrowing_history": {
        "last_10_ticks": [
            {
                "tick_group": "2026-07-12-14-05-00",
                "allocated": 61,
                "used": 58,
                "borrowed": 0,
                "lent": 3,
                "jobs": 6
            }
        ]
    },
    "updated_at": "2026-07-12T14:05:00Z"
}
```

---

## 7. States

### 7.1 Tick State Machine

```
                    ┌──────────────────────────────┐
                    │                              │
                    ▼                              │
 ┌────────┐    ┌─────────┐    ┌───────────┐       │
 │ QUEUED │───▶│ RUNNING │───▶│ COMPLETED │       │
 └────────┘    └─────────┘    └───────────┘       │
                    │                              │
                    ├──────────────────────────────┘
                    │          (timeout — 30 min)
                    ▼
              ┌──────────┐
              │ TIMED_OUT│
              └──────────┘
```

Transitions:
- `QUEUED → RUNNING`: Spawn engine successfully starts hermes chat subprocess
- `RUNNING → COMPLETED`: Process exits with code 0, session outcome queried successfully
- `RUNNING → FAILED`: Process exits with non-zero code or outcome query fails
- `RUNNING → TIMED_OUT`: Process exceeds spawn timeout (default 30 min), SIGKILL sent

### 7.2 Project States

- `enabled=1` + cooldown elapsed → eligible for scheduling
- `enabled=1` + cooldown not elapsed → skipped this tick
- `enabled=0` → soft-deleted, never scheduled
- No row → never registered

---

## 8. Errors

| Operation | Error | Cause |
|-----------|-------|-------|
| `CreateProject` | `UNIQUE constraint failed: projects.name` | Duplicate project name |
| `CreateTick` | `UNIQUE constraint failed: ticks.id` | Tick ID collision (clock went backwards) |
| `UpdateTickOutcome` | No rows affected | Tick ID not found |
| `GetProject` | `sql.ErrNoRows` | Project doesn't exist |
| Any write | `database is locked` | WAL checkpoint in progress; retry with 5s busy_timeout |
| `PruneOldTicks` | No error (0 rows) | No ticks to prune — success |

---

## 9. Testing

See S10 for full test spec. Key tests for data layer:

1. **Schema creation** — `NewStore(":memory:")` creates all tables and indexes
2. **Migration** — Running `Migrate()` twice is idempotent
3. **Project CRUD** — Create, read, update, soft-delete, list with filter
4. **Tick lifecycle** — Insert queued → transition to running → update outcome to completed
5. **Tick pruning** — Insert 250 ticks, prune to 200, verify count
6. **Concurrent access** — WAL mode allows concurrent reads during writes
7. **Foreign key** — Deleting a project cascades to its ticks
8. **Check constraints** — Weight 0 rejected, priority -1 rejected, invalid status rejected

---

## 10. Security

| Vector | Mitigation |
|--------|-----------|
| SQL injection | All queries use `?` placeholders via `database/sql` |
| Malicious project name | PK constraint limits length; regex validation in API layer |
| Tick ID collision | PK constraint catches it; generate with nanosecond precision |
| DuckBrain write access | Overwrite not append — limits blast radius of bad writes |
| Data loss | SQLite WAL mode + `PRAGMA synchronous=NORMAL` (balanced safety/perf) |
