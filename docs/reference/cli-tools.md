# Operator CLI reference — `cmd/` tools

Reader-facing reference for the command-line tools that ship in this repo's
`cmd/` tree. Each section is derived from the tool's own `main.go` — the source
of truth — and from the tool's `--help` probe, which stays reproducible: every
command below can be run in a fresh checkout with no state and no credentials.

| Tool | Binary | One line |
|------|--------|----------|
| [`cmd/migrate`](#cmdmigrate) | `bin/migrate` | Import Hermes cron jobs from `jobs.json` into the scheduler database |

Related pages: the daemon's own flags are tabulated in
[flags.md](flags.md) (defaults match `cmd/schedulerd/main.go`); every environment
variable the daemon reads is in [env-vars.md](env-vars.md); the other ad-hoc
`cmd/` tools (`backfill-commit-signals`, `queueprobe`) are described under
"Maintenance tools" in [README.md](../../README.md#maintenance-tools).

## `cmd/migrate`

Imports Hermes cron jobs from a `jobs.json` file into the scheduler SQLite
database, one project row per eligible job. It is a **one-shot** tool: it writes
project rows and exits; it never runs the daemon, never spawns a tick, and never
touches a project that already exists in the target database.

### Invocation

```bash
# 1. The help probe — always reproducible, exits 0, touches nothing
go run ./cmd/migrate --help

# 2. Wrapper targets from the Makefile (build + run with explicit paths)
make migrate-dry     # ./bin/migrate -jobs $(HOME)/.hermes/cron/jobs.json -db $(HOME)/.hermes/coding-hermes/scheduler.db --dry-run
make migrate         # same, without --dry-run

# 3. Direct invocation (any paths; nothing is hardcoded except the flag defaults)
./bin/migrate -jobs ~/.hermes/cron/jobs.json -db ~/.hermes/coding-hermes/scheduler.db --dry-run
```

The usage line prints the process's own path, so `go run` shows a temporary
build directory rather than `./cmd/migrate`:

```
Usage: <binary path> [flags]

Imports Hermes cron jobs from jobs.json into the scheduler database.
```

The tool takes **no positional arguments**. Everything it does is driven by the
four flags.

### Flags

Registered in `cmd/migrate/main.go:47-50`; the defaults below are expanded from
the environment (`os.ExpandEnv`) at flag-registration time, so an unset `$HOME`
would leave the literal `$HOME/...` text in place.

| Flag | Type | Default | Effect |
|------|------|---------|--------|
| `-jobs` | string | `$HOME/.hermes/cron/jobs.json` | Path to the cron `jobs.json` to read. The file is optional: absent = an empty import set (see below). |
| `-db` | string | `$HOME/.hermes/coding-hermes/scheduler.db` | Path to the scheduler SQLite database. Created (with parent directory and full schema) if it does not exist — including under `--dry-run`. |
| `-dry-run` | bool | `false` | Report what would be imported without writing project rows. Still opens/creates the database. |
| `-version` | bool | `false` | Print version/build info and exit — before the jobs file is read and before the database is opened. |

Parsing is the standard Go `flag` package, so all of `-jobs P`, `-jobs=P`,
`--jobs P`, `--jobs=P` work; a bare `--dry-run` means `--dry-run=true`; an
unknown flag or a missing flag value prints the usage block and exits **2**
(see [Exit codes](#exit-codes)).

`-version` prints one line and returns:

```
migrate <version> (commit: <sha>, built: <timestamp>)
```

The three values come from `internal/version` — the same build identity the
daemon reports on `/api/v1/health` and `schedulerd --version` (see
[flags.md](flags.md)). Because it returns before any I/O, `-version` is safe to
run against a live database path.

### Execution order

Per `cmd/migrate/main.go:57-155`, in this exact order:

1. Parse flags.
2. If `-version` → print and exit **0**. Nothing else runs.
3. **Load `jobs.json`** (`loadJobs`, `main.go:158-175`). The file must contain a
   wrapper object with a `jobs` array: `{"jobs": [...]}`. Per job the importer
   reads `id`, `name`, `skills`, `model`, `provider`, `enabled`,
   `schedule.display` and `prompt`; other fields are ignored.
   - **Absent file is not an error** (SCHED-GAP-184): a fresh machine with no
     `~/.hermes/cron/jobs.json` logs
     `jobs.json <path> not found — no cron jobs to migrate` and proceeds with
     zero jobs, exit **0**.
   - **Unreadable or unparseable file is fatal**: `FATAL: load jobs: ...`,
     exit **1** — and because this happens *before* the database is opened, a
     broken `jobs.json` never creates or migrates a database.
4. **Open/initialize the database** (`database.InitDB`, `main.go:77-81`): the
   parent directory is created (mode `0750`), the SQLite file is created if
   absent, WAL mode is enabled, and the schema migrations run. This is
   unconditional — **`--dry-run` still creates the database file**; it only
   suppresses project-row writes.
5. Per job, apply the [eligibility](#eligibility-and-skip-reasons) filters, then
   either preview (`--dry-run`) or `CreateProject` it.
6. Log the summary — `Done: <N> imported, <M> skipped` — and, under
   `--dry-run`, one extra line: `Run without --dry-run to perform the import.`

### Eligibility and SKIP reasons

Only jobs that pass **all** applicable filters are imported. The rules are
narrower than the help text's summary, so the code is quoted here with its
exact semantics:

| # | Rule | Source | Exact behavior |
|---|------|--------|----------------|
| 1 | Job name mentions the fleet | `main.go:183-184` | `strings.ToLower(name)` contains `coding-hermes` **or** `foreman`. Case-insensitive. |
| 2 | **or** a skill mentions the fleet | `main.go:177-182` | Any entry in `skills` contains the literal string `coding-hermes`. **Case-sensitive**, and `foreman` in a skill does **not** qualify — only the name check accepts `foreman`. |
| 3 | Prompt carries a workdir | `main.go:37,187-194` | Regex `[Ww]orkdir:\s*(\S+)` against `prompt`. The colon is **required**; `W`/`w` only; the captured path is everything up to the next whitespace, with trailing `.,;:` trimmed. |

Rules 1 and 2 are alternatives (name **or** any skill); rule 3 is mandatory on
top of them.

Measured matrix (built binary, 4-job fixture, `--dry-run`):

| Job | Why | Result |
|-----|-----|--------|
| name `myproj coding-hermes-foreman`, prompt `Workdir: /tmp/doc28/realdir` | name matches, colon form | imported |
| name `other coding-hermes-foreman`, prompt `workdir /tmp/doc28/realdir` | no colon after `workdir` | `SKIP other: no workdir found in prompt` |
| name `second job same dir`, skill `coding-hermes-docs`, prompt `Workdir: /tmp/doc28/realdir/` | skill matches (name does not) | imported (collapses into the same project, below) |
| name `alpha`, prompt `Workdir: /tmp/doc28/realdir` | neither name nor skill mentions the fleet | `SKIP alpha: not a coding-hermes job (name/skills must mention coding-hermes/foreman)` |

The workdir path is **not** existence-checked: `Workdir: /nonexistent/path`
imports. A trailing slash is preserved in the stored `workdir` value.

### What a real (non-dry) run writes

One `projects` row per eligible job, built in `main.go:129-140`:

| Column | Value |
|--------|-------|
| `name` | `filepath.Base(workdir)` — the directory's basename, **not** the job name (`main.go:196-210`) |
| `repo_url` | `local:<workdir>` |
| `workdir` | the path captured from the prompt |
| `weight` | `10` |
| `priority` | `5` |
| `cooldown_s` | `900` |
| `decay_rate` | `1.0` |
| `model` | job `model`, else `config.DefaultModel` (`internal/config/config.go:26`) |
| `provider` | job `provider`, else `config.DefaultProvider` (`internal/config/config.go:27`) |
| `enabled` | the job's `enabled` value — a disabled cron job imports as a disabled project |

**Name collapsing is the sharp edge:** because the key is the workdir basename,
several jobs pointing at one directory become one project. Measured on the
fixture above, the dry run reported two importable jobs and the real run wrote
one row, skipping the second with `SKIP realdir: already exists`. Plan for the
project count to be smaller than the eligible-job count.

### SKIP reasons

Every ineligible or already-imported job produces exactly one line naming the
job and the reason — there are no silent skips:

| Message | Cause |
|---------|-------|
| `SKIP <name>: not a coding-hermes job (name/skills must mention coding-hermes/foreman)` | rules 1 and 2 both failed |
| `SKIP <name>: no workdir found in prompt` | rule 3 failed (most often a missing colon or a missing workdir) |
| `SKIP <name>: already exists` | a project with that name is already in `-db` — **write mode only**; `--dry-run` does not check |
| `ERROR importing <name>: <err>` | `CreateProject` failed (logged, loop continues, run still exits 0) |

A skip is never fatal: the process still ends with exit **0** and the
`Done: N imported, M skipped` summary.

### `--dry-run` contract

`--dry-run` is a preview of *project rows*, not a stateless mode:

- It writes **no** project rows and reports each would-be import as
  `DRY-RUN: <name> (w=10 p=5 workdir=<path> model=<m> provider=<p> enabled=<bool> schedule=<display>)`.
- It **does** open and initialize the database at `-db` (creating the file,
  parent directory and schema when absent), so point `-db` at a scratch path if
  you want a guaranteed no-touch run.
- It **does not** check "already exists", so its `imported` count includes jobs
  a real run would skip — the dry-run number is an upper bound, never a
  prediction of rows written.
- It ends with `Run without --dry-run to perform the import.`

There is no `--apply` flag: omitting `--dry-run` **is** the write mode when the
tool is invoked directly. (`make migrate` is the writing target; `make
migrate-dry` adds `--dry-run`.)

Measured on the fixture, same `-db`: dry run `Done: 2 imported, 2 skipped`; then
the real run `Done: 1 imported, 3 skipped` (one `already exists`).

### Exit codes

The tool never calls `os.Exit` itself; `0`/`1` come from normal return and
`log.Fatalf`, and `2` from the `flag` package's own error path.

| Code | Meaning | Triggers |
|------|---------|----------|
| `0` | Completed | Successful import; a run where every job was skipped; an absent `jobs.json`; `-version`; `-h`/`--help` |
| `1` | Fatal | `jobs.json` unreadable or unparseable (`FATAL: load jobs: ...`); database init failure (`FATAL: database init: ...`) — e.g. an unwritable `-db` path |
| `2` | Usage error | Unknown flag, a flag missing its value, or an unparseable bool value — the usage block is printed, nothing runs |

Measured probes on the built binary:

```
$ ./migrate --help     ; echo rc=$?   → usage block, rc=0
$ ./migrate --bogus    ; echo rc=$?   → usage block + "flag provided but not defined: -bogus", rc=2
$ ./migrate -jobs /tmp/doc28/bad.json -db /tmp/doc28/nope.db ; echo rc=$?
                       → FATAL: load jobs: parse jobs.json: ... , rc=1, /tmp/doc28/nope.db NOT created
$ ./migrate -jobs /tmp/doc28/absent.json -db /tmp/doc28/absent.db --dry-run ; echo rc=$?
                       → "jobs.json ... not found — no cron jobs to migrate", rc=0
```

Because `SKIP` and `ERROR importing` do not change the exit status, **read the
summary line**, not the exit code, to learn whether anything was imported.

### Credentials

`cmd/migrate` reads **no** credentials and needs **none**: no environment
variable, token, gateway URL or API key is consulted, and nothing is sent over
the network. It touches exactly two configured paths — the `-jobs` file (read)
and the `-db` file (create/migrate/write, plus SQLite's `-wal`/`-shm`
companions). This page therefore documents no secret values, and there is
nothing to set before running it.

### Safety notes

- Run `--dry-run` first, and prefer a **copy** of a live database
  (`cp ~/.hermes/coding-hermes/scheduler.db /tmp/copy.db`) over the live file —
  `InitDB` sets WAL and runs migrations, which is not what you want against a
  database the daemon is serving.
- There is no undo and no delete path: the tool only creates project rows. To
  reverse an import, remove the rows through the API
  (`DELETE /api/v1/projects/{name}?confirm=true`) — see [../api.md](../api.md).
- The importer is idempotent by project name, so re-running it after fixing a
  `jobs.json` adds only the jobs that are new since the last run.
