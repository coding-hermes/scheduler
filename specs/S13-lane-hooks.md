# S13 — Lane Hooks (ordered pre/post scripts at global, namespace and lane scope)

**Status:** Draft
**Depends on:** S02 (data model), S05 (spawn engine), S06 (REST API), S07 (namespaces)
**Amends:** S12 §W6 (see §10 — the boundary between scheduler-owned lane worktrees and foreman-owned worker worktrees)
**Pages target:** 5-7

---

## 1. Overview

A lane tick today is a single fixed pipeline: claim a slot → resolve a model → call the
gateway → run → count commits → settle. Every fleet requirement discovered in operation
(prepare a checkout, refresh a branch, lease it, prove it idle, merge it back, sweep evidence,
reap the tree) has no place to live, so it is either baked into the lane's prompt (where an
agent that fails halfway leaves the tree wrong) or into a wrapper `command` (which replaces
the spawn, and with it the scheduler's view of tokens and cost).

S13 adds **lane hooks**: ordered command lists attached to a tick, declared at three levels
(GLOBAL, NAMESPACE, LANE), configured through one file and one API, and executed by the
scheduler at two phases (PRE, POST).

> **H0 — A hook is configuration, not code.** The scheduler executes whatever the operator
> declared, in the operator's order. There is no compiled-in list of "the hooks we support",
> and adding a step never requires a scheduler release.

> **H1 — The scheduler reads no hook state it did not write.** Definitions live in one file;
> the scheduler keeps only the resolved plan for the tick and the receipt of each step.

---

## 2. Interface

### 2.1 Types (`internal/lanehook/types.go`)

```go
package lanehook

// Phase is where a step runs relative to the agent tick.
type Phase string

const (
	PhasePre  Phase = "pre"
	PhasePost Phase = "post"
)

// Level is the scope a step is declared at.
type Level string

const (
	LevelGlobal    Level = "global"
	LevelNamespace Level = "namespace"
	LevelLane      Level = "lane"
)

// OnFailure decides what a non-zero exit does to the tick.
type OnFailure string

const (
	FailClosed  OnFailure = "closed"      // abort: the tick never spawns (pre) / is marked hook_failed (post)
	FailOpen    OnFailure = "open"        // record the failure, raise an event, continue
	FailRetry   OnFailure = "retry"       // retry up to Retries times, then apply FinalFailure
)

// Step is one entry as declared by the operator.
type Step struct {
	ID          string    `toml:"id"          json:"id"`
	Phase       Phase     `toml:"phase"       json:"phase"`
	Level       Level     `toml:"level"       json:"level"`
	Target      string    `toml:"target"      json:"target"`      // "*" for global, namespace id, or lane name
	Priority    int       `toml:"priority"    json:"priority"`    // lower fires first in PRE
	Command     string    `toml:"command"     json:"command"`     // absolute path, no shell
	Args        []string  `toml:"args"        json:"args"`        // placeholder-expanded, never shell-quoted
	TimeoutS    int       `toml:"timeout_s"   json:"timeout_s"`   // default 60, cap 600
	OnFailure   OnFailure `toml:"on_failure"  json:"on_failure"`  // default "closed" for pre, "open" for post
	Retries     int       `toml:"retries"     json:"retries"`     // only meaningful with on_failure="retry"
	RetryDelayS int       `toml:"retry_delay_s" json:"retry_delay_s"`
	Enabled     *bool     `toml:"enabled"     json:"enabled"`     // nil = true
}

// Plan is the resolved, ordered list for one tick.
type Plan struct {
	Lane      string `json:"lane"`
	Namespace string `json:"namespace"`
	Workdir   string `json:"workdir"`
	Pre       []Step `json:"pre"`
	Post      []Step `json:"post"`
}

// Env is the placeholder set available to every step.
type Env struct {
	TickID    string
	Lane      string
	Project   string
	Role      string
	Namespace string
	Workdir   string
	Repo      string
	Phase     Phase
	Deadline  time.Time
	HookID    string
}

// Runner executes a plan for one tick.
type Runner interface {
	// Pre runs the PRE list. A non-nil error means the tick must not spawn.
	Pre(ctx context.Context, e Env, plan Plan) error
	// Post runs the POST list. Errors are recorded, never propagated as tick failure.
	Post(ctx context.Context, e Env, plan Plan)
}

// Resolve builds the ordered plan for a lane. Pure: no I/O.
func Resolve(set []Step, lane, namespace, workdir, repo string, order Order) (Plan, error)

// Order selects the cross-level ordering rule.
type Order string

const (
	OrderLevelFirst Order = "level_first" // global→namespace→lane, each by priority asc
	OrderFlat       Order = "flat"        // one priority space across all levels
)
```

### 2.2 Resolution (normative)

```
PRE  = sort(global.pre) ++ sort(namespace.pre) ++ sort(lane.pre)     // each ascending
POST = sort(lane.post, desc) ++ sort(namespace.post, desc) ++ sort(global.post, desc)
```

- Sorting key everywhere: `(Priority, ID)` — a stable tie-break so two steps at the same
  priority never swap order between runs.
- `OrderFlat` instead sorts all PRE steps from all three levels by `(Priority, Level, ID)`
  and all POST steps descending by `(Priority, Level, ID)`.
- A step whose `Target` does not name this lane/namespace/`*`, or whose `Enabled` is false, is
  not in the plan.
- A lane's `skip_hooks` list removes steps by ID **after** resolution; removing an ID that is
  not in the plan is not an error (operators rename things).
- Duplicate `(Phase, Level, Target, ID)` is a **load error**, not a resolution error: the file
  is refused whole (see §5.2). Two different levels may carry the same ID; both run.

### 2.3 Execution (normative)

1. PRE runs **after** the slot is claimed and the `ticks` row exists (a step must be able to
   name its tick), and **before** model resolution and the gateway call.
2. Each step runs with `exec.CommandContext`, **no shell**, `cmd.Dir = project.Workdir`,
   `Setpgid: true`. On timeout or cancellation the whole **process group** is killed
   (`syscall.Kill(-pgid, SIGKILL)`), then reaped. A hook may not leave children behind.
3. The child environment is `os.Environ()` plus the §3.4 variables plus
   `CODING_HERMES_HOOK=1`. If the daemon sees `CODING_HERMES_HOOK=1` in its own environment it
   refuses to start (a hook that launches a scheduler is a loop).
4. `Args` and `Command` are placeholder-expanded from §3.4 (`{lane}`, `{namespace}`,
   `{workdir}`, `{repo}`, `{tick}`, `{phase}`), then passed as an argument vector. A step that
   wants a shell must name one (`command = "/bin/bash"`, `args = ["-c", "..."]`) — the runner
   never inserts one.
5. stdout and stderr are captured into one buffer, truncated to 64 KiB (head 32 KiB + tail
   32 KiB) and stored on the `hook_runs` row.
6. Total PRE time is bounded by `min(sum(timeout_s), tick_timeout − 300s)`. On exhaustion the
   remaining steps are written `skipped` and the tick fails `hook_budget_exhausted`.
7. POST runs **after** `CompleteTick` has written the outcome, so nothing a post step does can
   change the tick record. A POST failure never changes `ticks.outcome`.
8. `on_failure` is interpreted **per phase**, and the difference is normative:
   - PRE `closed` → the tick does not spawn; POST still runs (a lease taken in PRE is released).
   - PRE `open` → recorded, the tick spawns.
   - POST `closed` → **the remaining POST steps do not run** (receipt
     `state=skipped`, `reason=aborted_by:<id>`); the tick outcome is untouched.
   - POST `open` → recorded, the remaining POST steps still run.
   A POST list is a chain (gate → merge → prune); letting a failed gate fall through to a merge
   is the defect this rule exists to prevent.

---

## 3. Data model

### 3.1 Definitions — the file is the source of truth

`~/.hermes/hooks.toml` (path from `--hooks-file` / `SCHEDULER_HOOKS_FILE`, default
`$HOME/.hermes/hooks.toml`). Git-tracked; it is the only definition store. The scheduler
holds no copy of the definitions in the database, so a restore of the file restores the
fleet's hook set.

```toml
[scheduler]
hook_order = "level_first"          # "level_first" | "flat"
hook_total_budget_s = 900           # optional; default = tick_timeout - 300

[[hook]]
id = "lease-acquire"
phase = "pre"
level = "global"
target = "*"
priority = 5
command = "/home/kara/.hermes/hooks/lease-acquire.sh"
timeout_s = 30
on_failure = "closed"

# Per-lane declaration (S14 §2.1). skip_hooks is here rather than a project
# column on purpose: a column would be dropped by the fleet.toml generator,
# while this file is the definition store the daemon actually reads.
[lane."code-factory-qa"]
repo = "/home/kara/code-factory"
base = "main"
policy = "persistent"
push = true
floor_gib = 8.0
gates = ["make selftest"]
skip_hooks = ["repo-fetch"]

[[hook]]
id = "worktree-ensure"
phase = "pre"
level = "lane"
target = "code-factory-qa"
priority = 20
command = "/home/kara/.hermes/hooks/worktree-ensure.sh"
args = ["--lane", "{lane}", "--repo", "{repo}", "--worktree", "{workdir}"]
timeout_s = 120
on_failure = "closed"

[[hook]]
id = "lane-merge"
phase = "post"
level = "lane"
target = "code-factory-qa"
priority = 60
command = "/home/kara/.hermes/hooks/lane-merge.sh"
timeout_s = 240
on_failure = "open"
```

### 3.2 Receipts — `hook_runs`

```sql
-- tick_id carries NO foreign key: on the gateway path a PRE receipt is
-- written before the spawner inserts the tick row, so an FK would refuse the
-- first receipt of every hooked tick. The index is the join path instead.
CREATE TABLE IF NOT EXISTS hook_runs (
    run_id       INTEGER PRIMARY KEY AUTOINCREMENT,
    tick_id      TEXT NOT NULL,
    lane         TEXT NOT NULL,
    phase        TEXT NOT NULL CHECK(phase IN ('pre','post')),
    level        TEXT NOT NULL CHECK(level IN ('global','namespace','lane')),
    hook_id      TEXT NOT NULL,
    priority     INTEGER NOT NULL,
    seq          INTEGER NOT NULL,             -- position in the resolved plan, 1-based
    command      TEXT NOT NULL,
    state        TEXT NOT NULL CHECK(state IN ('running','ok','failed','timeout','skipped','busy')),
    exit_code    INTEGER NOT NULL DEFAULT 0,
    started_at   TEXT NOT NULL,
    finished_at  TEXT,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    attempts     INTEGER NOT NULL DEFAULT 1,
    output       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS hook_runs_tick_idx ON hook_runs(tick_id, phase, seq);
CREATE INDEX IF NOT EXISTS hook_runs_hook_idx ON hook_runs(hook_id, started_at DESC);
```

One row per attempt; `attempts` counts retries on the winning row and each failed attempt gets
its own row with `state='failed'` so a flapping step is visible. Exit `10` is the non-fatal
`state='skipped'` outcome; quiesce-check exit `11` is `state='busy'`, which remains subject
to the step's `on_failure` policy and therefore stops a closed POST chain.

### 3.3 Cache — resolved plans

`plan_cache` is **not** persisted. The plan is resolved per tick from the loaded set (a few
hundred entries at most, one sort) and exposed for the tick's lifetime; `GET /api/v1/hooks/plan`
resolves on demand. Nothing about a plan survives a restart, by design: the file is the truth.

### 3.4 Environment handed to every step

| Variable | Source |
|---|---|
| `CODING_HERMES_TICK` | `ticks.id` |
| `CODING_HERMES_PROJECT` | project name |
| `CODING_HERMES_LANE` | project name (alias, explicit) |
| `CODING_HERMES_ROLE` | lane role suffix (`foreman`, `qa`, `docs`, …) or `primary` |
| `CODING_HERMES_NAMESPACE` | project's `namespace_id` |
| `CODING_HERMES_WORKDIR` | project `workdir` (the tick's cwd) |
| `CODING_HERMES_REPO` | project `repo_url` with the `local:` prefix stripped |
| `CODING_HERMES_PHASE` | `pre` \| `post` |
| `CODING_HERMES_HOOK_ID` | the step's `id` |
| `CODING_HERMES_DEADLINE_AT` | RFC3339 of the tick deadline |
| `CODING_HERMES_HOOK` | `1` (reentrancy guard) |

---

## 4. Wiring

### 4.1 Flags and config

| Surface | Name | Type | Default | Validation |
|---|---|---|---|---|
| flag | `--hooks-file` | string | `$HOME/.hermes/hooks.toml` | missing file = zero hooks, INFO log, no error |
| env | `SCHEDULER_HOOKS_FILE` | string | above | env wins over flag's default |
| env | `SCHEDULER_HOOK_ORDER` | string | `level_first` | must be `level_first` or `flat`, else boot refuses |
| toml | `[scheduler].hook_order` | string | `level_first` | as above (env > toml) |
| toml | `[scheduler].hook_total_budget_s` | int | `tick_timeout − 300` | `> 0` and `< tick_timeout` |
| hooks.toml | `[lane."<name>"].skip_hooks` | `[]string` | empty | unknown IDs ignored |

`ProjectDef` gains **no** field: `skip_hooks` is declared in hooks.toml beside
the lane's worktree spec (`LaneSpec.SkipHooks`), because the fleet.toml
generator rewrites `[[projects]]` blocks from the DB and would silently drop a
column-less key. The lane's own row is untouched by hook configuration.

Reload: the file's mtime+size are stat'ed once per evaluation pass. A change reloads
atomically (parse to a new set, validate, swap); a parse error keeps the **previous** set
armed and emits `hook.config_rejected` with the reason. The file is never half-applied.

### 4.2 REST routes (`internal/api/server_hooks.go`, registered in the OpenAPI map)

| Method + path | Purpose |
|---|---|
| `GET /api/v1/hooks` | list definitions, optional `?phase=&level=&target=` |
| `GET /api/v1/hooks/plan?lane=<name>` | the resolved ordered plan, without running |
| `PUT /api/v1/hooks/{id}` | create or replace one step (validates the whole resulting file) |
| `DELETE /api/v1/hooks/{id}` | remove one step |
| `POST /api/v1/hooks/{id}/test` | run one step now against a named lane, capture output, no tick |
| `POST /api/v1/hooks/reload` | re-read the file, return the step count or the parse error |

Operator-token protected (`"security": [{"operatorToken": []}]`), like every other write.
`PUT`/`DELETE` write the file atomically (`tmp` + `rename`, mode 0644) and then reload, so the
API and a hand edit converge on the same bytes.

### 4.3 Spawn-path insertion (`internal/scheduler/spawn.go`)

```
claim slot
  → insert ticks row (status=running)
  → plan := lanehook.Resolve(...)
  → pre := runner.Pre(ctx, env, plan)          // NEW
  → if pre != nil: CompleteTick(outcome=failed, reason=hook_failed); release slot; return
  → resolve model/provider (router)
  → gateway or command spawn
  → CompleteTick(...)                          // counts commits/files in project.Workdir
  → runner.Post(ctx, env, plan)                // NEW — after the row is durable
```

`SetHookRunner` on `Loop` mirrors `SetSchedulerBus`: nil runner = feature off, byte-identical
behaviour. `sim_spawn.go` must call the same two seams so simulated ticks exercise hooks.

### 4.4 Events

`EventLogger.Emit(ctx, severity, "hook", <message>, details)`:

| message | severity | details |
|---|---|---|
| `hook.pre.start` / `hook.post.start` | info | `hook_id, tick_id, lane, level, priority, seq, timeout_s` |
| `hook.pre.ok` / `hook.post.ok` | info | `hook_id, tick_id, duration_ms, exit_code` |
| `hook.pre.fail` / `hook.post.fail` | high | `hook_id, tick_id, exit_code, attempts, output_head` |
| `hook.pre.timeout` | high | `hook_id, tick_id, timeout_s` |
| `hook.pre.veto` | critical | `hook_id, lane` — the tick did not spawn |
| `hook.config_rejected` | high | `file, error` |

---

## 5. Error catalog

| Condition | Result |
|---|---|
| hooks file missing | zero hooks; `INFO hooks: no file at <path>`; the fleet runs unchanged |
| hooks file unparseable / duplicate key / unknown enum | boot continues on the previous set; `hook.config_rejected`; API `PUT` returns `400` and leaves the file untouched |
| `command` not absolute, or not executable | load error for that step (`400` on `PUT`; boot keeps previous set) |
| PRE step exits non-zero, `on_failure=closed` | `ticks.outcome=failed`, `failure_reason=hook_failed:<id>`; slot released; the POST list still runs |
| PRE step exits non-zero, `on_failure=open` | recorded `failed`, event raised, tick continues |
| POST step exits non-zero, `on_failure=closed` | recorded `failed`, event raised; remaining POST steps `skipped` (`aborted_by:<id>`); `ticks.outcome` unchanged |
| POST step exits non-zero, `on_failure=open` | recorded `failed`, event raised, remaining POST steps continue |
| step exits non-zero, `on_failure=retry`, retries exhausted | behaves as `closed` for pre, `open` for post |
| step exceeds `timeout_s` | process group killed; `state=timeout`; policy applied |
| total PRE budget exhausted | remaining steps `skipped`; tick fails `hook_budget_exhausted` |
| daemon started with `CODING_HERMES_HOOK=1` | refuses to start: `hooks: refusing to run inside a hook` |
| `PUT /api/v1/hooks/{id}` with a body that would duplicate `(phase,level,target,id)` | `409` |
| `GET /api/v1/hooks/plan?lane=` for an unknown lane | `404` |
| hook writes to stdout > 64 KiB | truncated, `state=ok`, event notes `truncated=true` |

---

## 6. Edge cases

- **Concurrency.** GLOBAL PRE steps run once per tick and ticks run concurrently, so a global
  step must be idempotent or take its own lock. The runner exports `CODING_HERMES_HOOK` and
  `CODING_HERMES_TICK`; the ambient lock path is `/run/user/<uid>/coding-hermes-hooks/` and a
  step that mutates shared state is expected to `flock` there. The scheduler does not serialise
  global steps across ticks — serialising them would make the pool's 16 slots 1.
- **Slot accounting.** A vetoed PRE returns the slot immediately; the lane's cooldown is **not**
  advanced by a veto (the lane never ran, so its cadence must not be punished).
- **A hook that hangs the daemon.** Hooks run in their own process group with a hard timeout;
  the evaluation pass is never blocked on a hook (they run in the spawn goroutine).
- **Empty plan.** Zero steps is the normal case and must be free: no events, no rows, no log.
- **Restart mid-tick.** A tick's plan is not persisted; a resumed tick re-resolves. Steps must
  therefore be safe to run twice (that is the same contract the spec places on worktree steps).
- **Clock.** `CODING_HERMES_DEADLINE_AT` is the tick deadline, so a step can budget itself; the
  runner does not shorten `timeout_s` to fit it (a step that overruns the tick is a spec error,
  surfaced by the timeout receipt).
- **Security.** Hooks execute as the daemon's uid with the daemon's environment. The file is
  mode 0644 and git-tracked; not a secret store. Any credential a step needs comes from a 0600
  env file it reads itself, never from argv.

---

## 7. Testing

| # | Test | Expectation |
|---|---|---|
| T1 | `Resolve` with 2 global, 2 namespace, 3 lane PRE steps | order global(10,40) → ns(10,20) → lane(5,20,90); tie at same priority broken by ID |
| T2 | `Resolve` POST | exact reverse: lane(90,20,5) → ns(20,10) → global(40,10) |
| T3 | `OrderFlat` | one priority space; equal priority orders by level name then ID |
| T4 | duplicate `(phase,level,target,id)` in the file | load error naming both lines; previous set stays armed |
| T5 | `enabled=false` and `skip_hooks` | step absent from the plan; unknown skip ID is not an error |
| T6 | closed PRE step exits 1 | no gateway call (assert with a fake spawner), `ticks.outcome=failed`, `failure_reason=hook_failed:<id>`, slot released |
| T7 | open POST step exits 1 | `ticks.outcome` unchanged, `hook_runs.state=failed`, one high event |
| T8 | step sleeps past `timeout_s` and forks a child that sleeps | parent and child both dead within 1 s of the deadline (read `/proc` for the child pid) |
| T9 | `retry` step fails twice then succeeds | three `hook_runs` rows, winning row `attempts=3`, tick proceeds |
| T10 | PRE budget exhausted across steps | remaining steps `skipped`, tick failed `hook_budget_exhausted` |
| T11 | file edited while running | next pass reloads; a broken edit keeps the old set and emits `hook.config_rejected` |
| T12 | `PUT /api/v1/hooks/{id}` valid / duplicate / non-executable command | `200` with file bytes on disk / `409` / `400`, file unchanged in both error cases |
| T13 | `GET /api/v1/hooks/plan?lane=crier-qa` | byte-stable JSON; same body twice |
| T14 | daemon env carries `CODING_HERMES_HOOK=1` | boot fails with the documented message |
| T15 | zero steps | no `hook_runs` rows, no events, spawn path timing within 1 ms of the un-hooked path |
| T16 | `sim_spawn` tick with a closed pre-hook | same outcome as the real spawner (seam parity) |
| T17 | POST step 2 of 4 is `closed` and fails | steps 3-4 have no `hook_runs` rows; tick outcome unchanged |
| T18 | PRE `closed` failure with a lease taken in step 1 | the POST list runs, lease released |

---

## 8. Hilo impact

Depends on: `internal/config` (ProjectDef), `internal/database` (migrations, ticks),
`internal/scheduler` (spawn.go, tick_process.go, events.go, sim_spawn.go), `internal/api`.
Depended on by: **S14** (lane worktrees are an S13 hook set — see `specs/S14-lane-worktrees.md`), the fleet's `hooks.toml`, and
`fleet-sync.py` (which will mirror `skip_hooks` into the project blocks).

---

## 9. Out of scope

- Hooks on the **wave/worker** path (`tick_workers`). Workers are the foreman's business; S12
  W0 keeps the scheduler out of them.
- Hook execution on remote nodes. Hooks run on the control host; a bunker-side equivalent is a
  separate spec.
- Any hook DSL, conditionals or templating beyond the placeholder set in §3.4. A step that
  needs logic writes a script.

---

## 10. Amendment to S12 §W6

S12 W6 reads: *"The scheduler never writes into a worktree, never runs `git worktree`, and never
deletes a worker branch."*

That statement is about **worker** worktrees inside a tick, and it stands. S13/S14 add a second,
disjoint object: a **lane worktree**, created by an operator-declared hook for the tick's *own*
cwd, never for a worker.

Restated invariant, which replaces W6 as the normative sentence:

> **W6′ — The scheduler's spawn path never writes into a worktree and never manipulates worker
> branches. Hook scripts are the operator's authority: they may create, refresh, merge and
> prune the lane's own worktree, and their receipts are the evidence. A wave worker's tree is
> never touched by a hook.**

A judge auditing S12 should read W6 through this paragraph; a worker implementing S13/S14 must
not add any `git` call to the Go spawn path.
