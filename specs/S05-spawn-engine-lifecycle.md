# S05 — Spawn Engine + Tick Lifecycle

**Status:** Draft  
**Depends on:** S01, S02, S03, S04  
**Pages target:** 3-4

---

## 1. Overview

The spawn engine turns a packed scheduler decision into one running Coding Hermes foreman process. It launches `hermes chat` directly, captures the Hermes session ID from stdout, records the PID and start time, enforces the configured timeout, and exposes the active-process count used by the concurrency guard.

The lifecycle tracker owns the terminal half of a tick. It waits for the child process, distinguishes normal exit, failure, scheduler cancellation, and timeout, exports the completed Hermes session, parses measurable outcome data, and writes one terminal `TickOutcome` to SQLite.

Together the components implement the S02 state machine: `QUEUED → RUNNING → COMPLETED|FAILED|TIMEOUT`. A successful spawn means the process started, has a PID, is registered as active, and the database row is `running`. Completion means the child is reaped, the session outcome is classified, all terminal fields are persisted, and the active slot is released.

The spawn engine does not choose projects, calculate urgency, pack weight, retry failed foreman work, interpret free-form assistant prose, render the dashboard, or sync DuckBrain. SQLite remains authoritative; in-memory PID state exists only while the daemon is running.

---

## 2. Dependencies

| Dependency | Purpose | Failure Mode |
|-----------|---------|-------------|
| `os/exec` (stdlib) | Start, wait for, and query child processes without a shell | Tick fails with command error |
| `os`, `syscall` (stdlib) | Validate workdirs; send `SIGTERM` and `SIGKILL` | Validation or cleanup error is recorded |
| `context`, `time` (stdlib) | Scheduler cancellation, 5s capture bound, 30m tick timeout | Child is terminated and classified |
| `bufio`, `encoding/json` (stdlib) | Parse stdout and JSONL session exports | Fallback identity or failed outcome |
| `sync` (stdlib) | Protect active process state | Required for race-free operation |
| `Store` (S02) | Persist tick transitions and events | Spawn is rolled back by killing the child |
| `Config` (S01) | Supplies timeout and concurrency limit | Invalid configuration prevents startup |
| `hermes-agent` CLI | Runs foremen and exports sessions | Tick is marked `failed` |

Defaults are `SpawnTimeout=30m` (`1800s`), `MaxConcurrent=8`, and `LoopInterval=60s`. The binary is resolved through the daemon's controlled `PATH`; shell startup files are not sourced.

Operational files remain under `~/.hermes/coding-hermes/`: `scheduler.db`, `projects.json`, and `dashboard.html`. Only `scheduler.db` is written by this component.

---

## 3. Interface

The following Go types and signatures are normative:

```go
package scheduler
import (
    "context"
    "os/exec"
    "time"
)
type RunningTick struct {
    Project, TickID, SessionID string
    PID                        int
    StartTime                  time.Time
    Cmd                        *exec.Cmd
}
type SpawnEngine struct {
    timeout       time.Duration
    maxConcurrent int
    active        map[string]*RunningTick
}
func NewSpawnEngine(timeout time.Duration, maxConcurrent int) *SpawnEngine
func (s *SpawnEngine) Spawn(ctx context.Context, p Project, tickID string) (*RunningTick, error)
func (s *SpawnEngine) Active() int
func (s *SpawnEngine) Kill(tickID string) error
type TickOutcome struct {
    Status, Outcome       string
    ExitCode              int
    Commits, FilesChanged int
    TokensIn, TokensOut   int
    CostUSD               float64
    Error                 string
}
type LifecycleTracker struct {
    store *Store
}
func NewLifecycleTracker(store *Store) *LifecycleTracker
func (l *LifecycleTracker) CompleteTick(ctx context.Context, rt *RunningTick) (*TickOutcome, error)
```
The implementation adds private synchronization around `active` and pending reservations. The lock is never held during waits, signals, pipe reads, or database I/O. The constructor initializes the map; invalid limits safely refuse spawns. The scheduler creates `queued`, the spawn path writes `running`, and `CompleteTick` writes exactly one terminal transition through a package-private Store transition callback.

---

## 4. Behavior

### 4.1 Exact Spawn Command

```text
hermes chat --quiet -q <prompt> -m <model> --provider <provider> -s coding-hermes-foreman -s coding-hermes-cron -s hilo-usage -s gitreins --workdir <workdir>
```

No shell is involved. Arguments are separate values in this exact order:

```go
args := []string{
    "chat", "--quiet",
    "-q", prompt,
    "-m", p.Model,
    "--provider", p.Provider,
    "-s", "coding-hermes-foreman",
    "-s", "coding-hermes-cron",
    "-s", "hilo-usage",
    "-s", "gitreins",
    "--workdir", p.Workdir,
}
cmd := exec.CommandContext(processCtx, "hermes", args...)
cmd.Dir = p.Workdir
```

`prompt` is a scheduler-generated, self-contained foreman instruction naming `p.Name` and `tickID`. It directs the foreman to inspect the project's task board, execute one bounded useful unit, run relevant verification, and report committed, dry-run, or failed status. Prompt and project values are argv content, never shell syntax.

### 4.2 Spawn Sequence

```text
1. Reject empty project name, tick ID, model, or provider.
2. Under lock, reject a duplicate tick ID or reserve failure at maxConcurrent.
3. os.Stat(p.Workdir): require an existing directory.
4. Derive processCtx with SpawnEngine.timeout; timeout starts before Start.
5. Build the exact []string arguments; set cmd.Dir to the workdir.
6. Acquire stdout pipe; attach a bounded stderr tail buffer.
7. Start the command. On failure, release the reserved slot and mark failed.
8. Capture PID and StartTime=time.Now().UTC().
9. Create RunningTick and register active[tickID] under lock.
10. Persist status=running and spawned_at=StartTime.
11. Capture session ID for at most 5 seconds while draining stdout.
12. Persist the real session ID or PID fallback; return RunningTick immediately.
```

The reservation prevents two simultaneous calls from claiming the last slot. `active` must never exceed `maxConcurrent`, including under `go test -race`. If `Start` succeeds but the database running transition fails, terminate and reap the process before returning; never leave an invisible child.

### 4.3 Session ID Capture

Scan stdout for the first line matching:

```text
^Session ID:\s*(\S+)\s*$
```

Group 1 is the opaque session ID. Empty IDs, wrong prefixes, and whitespace-only values do not match. Later matches are ignored. Capture ends at the first match, stdout close, process cancellation, or five seconds.

```go
select {
case line := <-lines:
    // parse; continue until match or channel close
case <-time.After(5 * time.Second):
    // use fallback
case <-processCtx.Done():
    // use fallback and continue cancellation cleanup
}
```

Fallback identity is `pid-<PID>`, for example `pid-43127`. It allows PID-based lifecycle tracking but is not proof that a Hermes session exists. Persist it, append a `warn` event, and still attempt session export later.

One goroutine must continue draining stdout after capture. Without draining, output can fill the pipe and deadlock the child. Non-session output is discarded or retained only in a bounded diagnostic tail; it is never stored wholesale.

### 4.4 Completion and Outcome Query

`CompleteTick` performs exactly one `Cmd.Wait()`. It validates `rt`, derives `ProcessState.ExitCode()`, queries only after successful non-timeout exit, persists the terminal outcome, and guarantees active-map cleanup.

The exact export command is:

```text
hermes sessions export --session-id <id> --format jsonl --dry-run
```

It is also direct argv execution:

```go
args := []string{
    "sessions", "export",
    "--session-id", rt.SessionID,
    "--format", "jsonl",
    "--dry-run",
}
cmd := exec.CommandContext(queryCtx, "hermes", args...)
output, err := cmd.Output()
```

The query context is bounded to `min(30s, remaining parent deadline)`. Command failure, invalid JSONL, negative metrics, or missing terminal data triggers one context-aware retry after 500ms. A second failure marks the tick failed. Each non-empty line must be JSON; unknown records/fields are ignored. Accept snake_case totals and equivalent nested usage totals.

```text
commits       = latest explicit session total, default 0
files_changed = latest explicit session total, default 0
tokens_in     = explicit total, else sum per-call input tokens
tokens_out    = explicit total, else sum per-call output tokens
cost_usd      = explicit total, else sum per-call costs
outcome       = explicit terminal outcome;
                else committed when commits > 0;
                else dry_run after successful exit
```

Never infer metrics from assistant prose. Unknown future fields are harmless; metrics must be finite and non-negative.

Completion order is:

```text
1. Wait for chat and capture exit code.
2. If deadline/cancellation caused exit, skip export and classify directly.
3. If exit code is non-zero or signaled, classify failed.
4. Otherwise export, retry once if necessary, and parse.
5. Build TickOutcome using Section 6 guards.
6. In one transaction, write terminal fields and completed_at=UTC now.
7. Append completion/failure event.
8. Reap child, finish stdout drain, and remove active entry on every path.
9. Return the persisted outcome and any operational error.
```

A duplicate completion call returns an already-completed error and does not call `Wait` or overwrite the first terminal row.

### 4.5 Timeout, Kill, and Shutdown

On the per-tick deadline:

```text
1. Record timeout as the cause.
2. Send SIGTERM to the child or its dedicated process group.
3. Wait up to 10 seconds.
4. If still alive, send SIGKILL.
5. Call Wait to reap it.
6. Persist status=timeout, outcome=timeout, and the timeout duration.
```

`Kill(tickID)` uses the same escalation. On scheduler `SIGTERM` or `SIGINT`, the top-level context is cancelled, active ticks are copied under lock, then signaled without holding the lock. Scheduler cancellation is `failed`, not `timeout`, because the configured tick deadline did not expire.

### 4.6 Edge Cases

| Scenario | Required Behavior |
|----------|-------------------|
| `hermes` is absent | Mark failed; error contains exactly `hermes: command not found`; active slot released |
| Workdir does not exist or is not a directory | Mark failed before spawn; do not attempt child process |
| Child is killed externally | `Wait` error/signaled state becomes failed with exit detail |
| Session line is valid | Store first non-empty captured ID |
| Session parse fails or stdout is empty | Use `pid-<PID>` and append warning |
| No ID within 5s | Use fallback and continue draining stdout |
| Outcome export fails once | Retry once after 500ms |
| Outcome export fails twice | Mark failed, regardless of chat exit zero |
| Max concurrent reached | Return typed error; caller handles next evaluation |
| Scheduler context cancelled | Graceful kill; mark failed with shutdown reason |
| Tick deadline expires | TERM→KILL if needed; mark timeout |
| Running-state DB write fails | Kill and reap child; return combined error |
| Duplicate active tick ID | Reject without replacing existing process |

---

## 5. Data

`RunningTick` is memory-only. `Cmd` and PID are not serialized. `StartTime` is UTC and mirrors `ticks.spawned_at`. `SessionID` transitions once from empty to a real ID or PID fallback and is then immutable.

Lifecycle writes the S02 tick table as follows:

| Phase | Fields |
|-------|--------|
| Queued | `id`, `project`, `status=queued`, `urgency`, `weight_used` |
| Spawned | `status=running`, `spawned_at`, `session_id` |
| Completed | `status=completed`, `outcome=committed|dry_run`, `completed_at`, exit and metrics |
| Failed | `status=failed`, `outcome=failed`, `completed_at`, `exit_code`, `error` |
| Timed out | `status=timeout`, `outcome=timeout`, `completed_at`, `exit_code`, `error` |

Terminal status and outcome are written atomically. Missing optional metrics become zero only after valid export parsing; parser failure is never converted to zero-valued success.

Events use S02 levels. Start and successful completion are `info`; PID fallback is `warn`; process, query, persistence, and timeout failures are `error`. Detail JSON may contain tick ID, PID, session ID, timeout, and exit code. It must not contain the full prompt, stdout, stderr, environment, or exported messages.

---

## 6. States

### 6.1 Tick State Machine

```text
QUEUED ──Start succeeds + running row persisted──▶ RUNNING
   │                                                 │
   └──validation/Start failure──▶ FAILED             ├──exit 0 + export valid──▶ COMPLETED
                                                     ├──nonzero/signal/export failure──▶ FAILED
                                                     └──tick deadline──▶ TIMEOUT
```

Terminal guards are evaluated in this order:

```text
1. Tick deadline caused termination => status=timeout, outcome=timeout.
2. Scheduler cancellation caused termination => status=failed, outcome=failed.
3. Exit code != 0 or signal => status=failed, outcome=failed.
4. Both exports fail or parsing fails => status=failed, outcome=failed.
5. Explicit committed outcome or commits > 0 => status=completed, outcome=committed.
6. Otherwise successful valid export => status=completed, outcome=dry_run.
```

Terminal rows are immutable. Active membership begins after successful `Start` and ends after the process is reaped and terminal persistence is attempted.

### 6.2 Process State

```text
UNSTARTED → STARTED → REAPED
                    ↘ TERM_REQUESTED → REAPED
                                      ↘ KILL_REQUESTED → REAPED
```

A child is never removed from `active` merely because a signal was sent. It remains active until reaped or positively unavailable, preventing zombies and inaccurate concurrency counts.

---

## 7. Errors

| Condition | Persisted Result | Recovery |
|-----------|------------------|----------|
| Invalid workdir/model/provider | `failed/failed` with field-specific error | Correct project configuration |
| `hermes` lookup/start failure | `failed/failed`; command error | Install Hermes or fix daemon `PATH` |
| Max concurrency | No spawn; typed `max concurrent ticks reached` error | Retry next evaluation |
| Duplicate active ID | Existing process unchanged | Generate unique tick ID |
| External kill/nonzero exit | `failed/failed` with exit/signal detail | No automatic retry |
| Scheduler shutdown | `failed/failed` with cancellation reason | Future cycle may reschedule |
| Tick timeout | `timeout/timeout` | Investigate workload |
| Session capture failure | Warning only; PID fallback | Export may still succeed |
| Export fails twice/malformed JSONL | `failed/failed` | Inspect Hermes exporter |
| Terminal database write failure | Return error; startup reconciliation repairs stale running row | Alert operator |
| Signal failure | Best-known terminal data plus signal error | Recheck process and alert |

Errors wrap causes with `%w` where applicable. Persisted error text is single-line where possible and limited to 4 KiB; stderr retains only a final 8 KiB diagnostic tail. If process and database operations both fail, return a joined error preserving both. Never report completion when terminal persistence failed.

---

## 8. Testing

Use Go `testing`, a temporary SQLite Store, and a mock `hermes` executable placed first on test-only `PATH`. Production code contains no test-only branches.

```go
func TestSpawn_WritesRunningTick(t *testing.T)          // running row, PID, start, active=1
func TestCompleteTick_PersistsAllFields(t *testing.T)   // commits/files/tokens/cost exact
func TestCompleteTick_DryRun(t *testing.T)              // exit 0, valid export, zero commits
func TestCompleteTick_Timeout(t *testing.T)             // exceeds timeout, TERM/KILL, active=0
func TestSpawn_CommandNotFound(t *testing.T)             // exact command-not-found error
func TestParseSessionID(t *testing.T)                    // valid, invalid, empty, first wins
func TestSpawn_SessionFallbackAndDrain(t *testing.T)     // pid fallback, no pipe deadlock
func TestSpawn_MaxConcurrent(t *testing.T)               // N running rejects N+1
func TestSpawn_ConcurrentLimitRace(t *testing.T)         // one remaining slot under -race
func TestCompleteTick_OutcomeRetry(t *testing.T)         // first export fails, second succeeds
func TestCompleteTick_OutcomeFailsTwice(t *testing.T)    // exit 0 still becomes failed
func TestCompleteTick_ExternalKill(t *testing.T)         // failed state and active cleanup
func TestShutdown_KillsRunningProcesses(t *testing.T)    // cancel, TERM, reap, failed, active=0
```

Required integration flow:

```text
1. Create temporary workdir and SQLite database.
2. Insert project with model/provider and a queued tick.
3. Put mock hermes first on PATH.
4. Mock chat prints `Session ID: test-session-123`, records argv, exits 0.
5. Mock export emits deterministic JSONL totals.
6. Spawn; verify every argv flag and order plus running database fields.
7. Complete; verify every TickOutcome and terminal SQLite field.
8. Verify start/completion events, Active()==0, and no child process remains.
```

Run `go test ./...` and `go test -race ./internal/scheduler/...`. Tests use injected short timeouts and never invoke the user's real Hermes binary or read `~/.hermes`.

---

## 9. Security

| Vector | Mitigation |
|--------|------------|
| Shell injection | `exec.CommandContext` with separate argv values; never `sh -c` |
| Binary substitution | Controlled systemd `PATH`; deployment may pin absolute Hermes path |
| Malicious workdir | Require existing directory and set `cmd.Dir`; do not create or clone here |
| Session/secret leakage | Store only bounded metrics/errors; never persist messages or environment |
| Descendant escape | Give each tick a process group and signal the group on shutdown |
| PID reuse | Retain `*os.Process`; never rediscover or authorize by PID alone |
| Resource exhaustion | Enforce concurrency, timeout, bounded buffers, and JSONL line limits |
| SQL injection | Parameterized Store methods; IDs are values, never SQL fragments |

On Unix, use a dedicated process group (`Setpgid: true`) so child tool processes cannot survive the foreman. Session IDs are opaque argv values, never paths, command names, URLs, or SQL. Do not log `cmd.Env`, full prompts, stdout, or exported session content.

---

## 10. Performance

| Metric | Target |
|--------|--------|
| Spawn entry to PID capture | < 500ms p99 locally |
| `Active()` | < 10µs |
| Session recognition after matching line | < 1ms |
| Session fallback bound | 5s |
| Export attempt | < 30s |
| Timeout to reap | timeout + at most 10s grace |
| Active children | Never exceed configured limit, default 8 |
| Scheduler overhead for 8 ticks | < 1MiB excluding children |
| Terminal database update | < 20ms p99 under WAL |

Map operations are O(1). Parsing is O(stdout lines before ID + export bytes). JSONL is streamed, not loaded without bounds. Persisted errors are capped at 4 KiB and stderr tails at 8 KiB. The engine never busy-polls PIDs: it uses `Wait`, contexts, timers, and channels. Shutdown work is O(active ticks), bounded by `MaxConcurrent`.
