# Operator CLI reference — `cmd/` tools

Reader-facing reference for the command-line tools that ship in this repo's
`cmd/` tree. Each section is derived from the tool's own `main.go` — the source
of truth — and from the tool's `--help` probe, which stays reproducible: every
command below can be run in a fresh checkout with no state and no credentials.

| Tool | Binary | One line |
|------|--------|----------|
| [`cmd/migrate`](#cmdmigrate) | `bin/migrate` | Import Hermes cron jobs from `jobs.json` into the scheduler database |
| [`cmd/scheduler-query`](#cmdscheduler-query) | `scheduler-query` | Federation read client — ask one peer, the local fan-out, or the daemon's authoritative aggregate |
| [`cmd/scheduler-dispatch`](#cmdscheduler-dispatch) | `scheduler-dispatch` | Hand ONE unit of work to ONE named agent over the Crier relay's durable inbox |

Related pages: the daemon's own flags are tabulated in
[flags.md](flags.md) (defaults match `cmd/schedulerd/main.go`); every environment
variable the daemon reads is in [env-vars.md](env-vars.md); `scheduler-query`'s
envelope contract is [../federation-query-spec.md](../federation-query-spec.md)
and `scheduler-dispatch`'s is [../dispatch-spec.md](../dispatch-spec.md); the
remaining ad-hoc `cmd/` tools (`backfill-commit-signals`, `queueprobe`) are
described under "Maintenance tools" in [README.md](../../README.md#maintenance-tools).

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

## `cmd/scheduler-query`

`scheduler-query` is the federation **read** client (REMOTE-011;
[../federation-query-spec.md](../federation-query-spec.md) §3, the "CLI" row).
By contract it is a thin adapter: it parses its surface, builds the §2.1 Query
envelope, hands it to the same transport entry the daemon's aggregate consumes
(`bus.Client.Query`), and renders the §2.2 Response envelope **verbatim**. It
computes no answer, caches nothing, writes nothing to the database and never
runs the daemon.

Sources: `cmd/scheduler-query/main.go` (surface: positionals, flags, environment
resolution), `cmd/scheduler-query/help.go` (the help block, `usageText`), and
`cmd/scheduler-query/query.go` (the three modes, rendering, exit codes).

| Shape | Invocation | Transport | Answer |
|-------|-----------|-----------|--------|
| one peer (default) | `scheduler-query <peer> <op> [flags]` | Crier relay: subscribe on the correlated reply topic, then `POST /relay/publish` (`internal/bus/query.go:255-320`) | the peer's own envelope, rendered |
| local fan-out | `scheduler-query --legacy-all <op> [flags]` | the same relay, one leg per peer in the local registry, ≤8 in flight (`query.go:182-285`) | **unmerged**; the interim fallback |
| authoritative aggregate | `scheduler-query --all <op> [flags]` | one `POST {daemon}/api/v1/federation/query` (`query.go:356`) | the **daemon's** fan-out + merge, printed verbatim |

### Invocation

`make build` produces only `bin/schedulerd` and `bin/migrate`, so build this
tool on its own:

```bash
# The help probe — reproducible in a fresh checkout, touches nothing.
# The block goes to STDERR and the process exits 0.
go run ./cmd/scheduler-query --help

go build -o /tmp/scheduler-query ./cmd/scheduler-query/
```

The usage stanza of that probe, verbatim (measured; exit status **0**):

```
scheduler-query — federation query client (REMOTE-011, docs/federation-query-spec.md §3 "CLI" row)

Usage:
  scheduler-query <peer> <op> [flags]     ask ONE peer
  scheduler-query --all <op> [flags]      the AUTHORITATIVE aggregate (REMOTE-012,
                                          docs/federation-query-spec.md §5) — delegates
                                          to the daemon's fan-out+merge and prints the
                                          merged §2.2 envelope (every peer's own status;
                                          a silent peer is a named row, never "down")
  scheduler-query --legacy-all <op>       the interim LOCAL bus fan-out, unmerged —
                                          kept as fallback for a daemon without the
                                          aggregate
```

Typical invocations:

```bash
# ask ONE peer
/tmp/scheduler-query peer-a peer.status
/tmp/scheduler-query peer-a events.list --arg since=2026-11-07T00:00:00Z --arg limit=20
/tmp/scheduler-query peer-a projects.list --arg filter=auger --json

# the LOCAL fan-out (unmerged; needs a populated peer registry)
/tmp/scheduler-query --legacy-all fleet.status

# the AUTHORITATIVE aggregate (the daemon fans out and merges)
/tmp/scheduler-query --all fleet.status
/tmp/scheduler-query --all fleet.status --want partial --budget-ms 2500 --json
```

Flags may precede or follow the positionals: `splitPositionals`
(`main.go:86,198-227`) hoists non-flag tokens out of argv before `flag.Parse`,
because Go's `flag` stops at the first non-flag token and the spec's CLI row puts
flags after the positionals. `--` ends flag parsing; everything after it is
positional.

### Positionals

| Position | Meaning |
|----------|---------|
| `<peer>` | the answering scheduler's id, as registered in the local peer registry |
| `<op>` | the read-catalogue op (table below) |

Arity is mode-dependent (`main.go:101-119`):

| Mode | Positionals | Wrong count |
|------|-------------|-------------|
| single (default) | exactly 2 — `<peer> <op>` | `scheduler-query: expected: scheduler-query <peer> <op> [flags]  (or --all <op>)` + the usage block, exit 2 |
| `--all` | exactly 1 — `<op>` | `scheduler-query: --all takes exactly one positional: <op>` + usage, exit 2 |
| `--legacy-all` | exactly 1 — `<op>` | `scheduler-query: --legacy-all takes exactly one positional: <op>` + usage, exit 2 |

An empty `<op>` is refused separately (`main.go:120-122`). `<op>` is **not**
validated against the catalogue: the surface only rejects emptiness, so a typo
travels to the peer (which refuses it and the run degrades to exit 1) rather
than failing as a usage error. `help.go:14-17` states the catalogue must stay in
lockstep with `internal/api`'s federation catalogue and the MCP tool map; the
shared entry point names the authoritative catalogue if the three ever drift.

### Read catalogue

| Op | Args (as the help block states them) |
|----|--------------------------------------|
| `peer.status` | — identity, version, clock, load, uptime of the peer |
| `fleet.status` | — peers, projects, active ticks, budget of the peer |
| `projects.list` | `filter=<exact name>` |
| `queue.get` | — the peer's ordered scheduling queue |
| `ticks.list` | `since=<RFC3339>` `limit=<int>` |
| `events.list` | `since=<RFC3339>` `limit=<int>` `severity=<string>` |

`--arg` values are carried as **strings** (`main.go:57-78`): `--arg limit=5`
sends `"limit":"5"`, not `5`. The shared entry point parses numeric and
RFC3339 forms itself (`argInt`, `argSince` —
`internal/api/server_federation.go:684-713`). An `--arg` without a `=` is a flag
parse error (exit 2), never a silently dropped argument.

### Flags

| Flag | Type | Default | Effect | Source |
|------|------|---------|--------|--------|
| `--arg k=v` | string, repeatable | none | op argument; must contain `=` | `main.go:71-78,97` |
| `--want MODE` | string | `""` (peer default `answer`) | `answer` (all-or-error) or `partial` (best effort with named gaps); anything else is exit 2 | `main.go:90,126-128` |
| `--budget-ms N` | int | `0` | per-peer read budget; `>0` arms the envelope's `budget_ms` and sizes the local waits; `≤0` means peer default (10 000 ms locally) | `main.go:91`, `query.go:86,196-200,299-302` |
| `--corr-id ID` | string | `""` → minted | correlation/idempotency id; minted ids look like `q-<scheduler-id>-<unixnano>-<016 counter>` | `main.go:92`, `internal/bus/query.go:171-174` |
| `--all` | bool | `false` | the authoritative aggregate via the daemon's `/api/v1/federation/query` | `main.go:88` |
| `--legacy-all` | bool | `false` | the interim local fan-out over the registered peers (no merge) | `main.go:89` |
| `--daemon-url URL` | string | `""` → `SCHEDULER_URL` → `http://127.0.0.1:9090` | daemon base URL for `--all`. **Pass it as `--daemon-url=URL`** — the space-separated form is refused (discrepancy 1) | `main.go:94,149-155` |
| `--json` | bool | `false` | print the envelope(s) only, one compact JSON object per line (NDJSON) | `main.go:93` |
| `--url URL` | string | `""` → `CRIER_URL` → `http://127.0.0.1:8767` | Crier relay base URL | `main.go:95,133-139` |
| `--db PATH` | string | `$HOME/.hermes/coding-hermes/scheduler.db` | scheduler database holding the peer registry; used by `--legacy-all` and for `last_contact` rendering | `main.go:96,145-148,53-55` |
| `-h`, `--help` | — (built-in) | — | print the block on **stderr** and exit **0** | `main.go:98`, `help.go:83-85` |

Precedence everywhere is flag → environment → default. With both `--all` and
`--legacy-all` set, `--all` wins silently (`main.go:104-113`).

Parsing is the standard `flag` package, so `-flag V`, `-flag=V`, `--flag V` and
`--flag=V` all work **except** for `--daemon-url V` (discrepancy 1). An unknown
flag, a missing value or a malformed `--arg` prints the flag error plus the usage
block and exits 2.

### Environment

| Variable | Read at | Effect | Overridden by |
|----------|---------|--------|---------------|
| `CRIER_URL` | `main.go:135` | relay base URL for single-peer and `--legacy-all` queries | `--url` |
| `CRIER_AUTH_TOKEN` | `main.go:140` | relay bearer credential, sent as `Authorization: Bearer …` | — (no flag, by design) |
| `SCHEDULER_ID` | `main.go:141-144` | caller identity: prefix of minted correlation ids and the relay's `X-Agent-ID`; unset falls back to the short hostname (`database.ResolveDefaultSchedulerID`, `"unknown"` if the hostname is unavailable) | — |
| `SCHEDULER_URL` | `main.go:151` | daemon base URL for `--all` | `--daemon-url` |
| `SCHEDULER_OPERATOR_TOKEN` | `query.go:362` | operator credential for the daemon's gate, sent as `X-Operator-Token` on `--all` **only** | — |
| `HOME` | `main.go:53-55` | expanded into the default `--db` path once at startup | `--db` |

Those five are the complete set: `grep os.Getenv cmd/scheduler-query/*.go` returns
exactly these reads. The `X-Operator-Token` header is set on every `--all`
request **even when the variable is empty** (`query.go:362`), so a gated daemon
answers 401 rather than "missing header".

### What each shape puts on the wire

**One peer (default).** One WebSocket subscribe on `fed.reply.<sanitized corr
id>` happens *before* the publish (the subscribe-before-publish window), then
`POST {relay}/relay/publish` with `{"topic":"fed.query.<peer>",
"event":<§2.1 envelope>,"reply_topic":"fed.reply.<…>"}`; the reply is matched on
the payload `corr_id`, never on arrival order (`internal/bus/query.go:255-320`).
The wait is `budget_ms` when positive, else 10 000 ms
(`queryDefaultBudgetMS`, `internal/bus/query.go:64`). `--db` is opened only to
render `last_contact` on a degraded human row (`query.go:163-167`); a missing
database is reported as `unknown` and is **never created**.

**`--legacy-all`.** Reads the registry's peers (`database.ListPeers`) from
`--db`; an absent database, an unreadable one, or an empty registry is a hard
error before any leg runs (`query.go:506-531`) — measured: no database file is
created on that path. Every leg mints its own correlation id (`query.go:219`),
at most 8 legs are in flight (`fanoutConcurrency`, `query.go:97`), and the
whole run is capped at `budget + 5000 ms` (`query.go:92,196-200`). A one-line
note on **stderr** names the authoritative aggregate so stdout stays parseable
(`query.go:194`).

**`--all`.** A single `POST {daemon}/api/v1/federation/query` with the §2.1
member set; `args.op` carries the read op, `want` and `budget_ms` appear only
when set (`query.go:318-346`). Measured body for `--all fleet.status --arg
limit=3` (stub daemon):

```
{"args":{"limit":"3","op":"fleet.status"},"corr_id":"q-<scheduler-id>-<unixnano>-0000000000000001","op":"fleet.aggregate"}
```

The answer is read with a 4 MiB cap (`query.go:369`). `401`/`403`/`503` name the
refusal and point at `SCHEDULER_OPERATOR_TOKEN`; any other non-200, an
unreachable daemon, or a body that is not a §2.2 envelope is a hard error
(exit 2, `query.go:363-386`). A well-formed aggregate whose `status` is not `ok`
exits 1 (`query.go:387-391`).

### Output

Default human mode for one peer — a summary line, then the envelope indented
(`query.go:460-475`). Measured against a dead relay port:

```
scheduler-query: peer=auger op=peer.status status=error as_of= age_ms=0 gaps=0 code=transport_error last_contact=unknown
{
  "corr_id": "q-karaHermes-mde-7840hs-1794047763651183303-0000000000000001",
  "op": "peer.status",
  "peer": "auger",
  "status": "error",
  "as_of": "",
  "age_ms": 0,
  "data": null,
  "gaps": [],
  "error": {
    "code": "transport_error",
    "message": "bus: query peer.status corr …: bus: dial 127.0.0.1:1: dial tcp 127.0.0.1:1: connect: connection refused"
  },
  "contract": "1.0.0"
}
```

`--json` prints the envelope **only**, one compact line (NDJSON for scripts) —
measured, same query with `--corr-id fixed-corr-1`:

```
{"corr_id":"fixed-corr-1","op":"peer.status","peer":"auger","status":"error","as_of":"","age_ms":0,"data":null,"gaps":[],"error":{"code":"transport_error","message":"…"},"contract":"1.0.0"}
```

`--all` in human mode — the summary line, one line per peer row the aggregate
returned, then the merged envelope indented (`query.go:398-427`); measured
against a canned daemon stub:

```
scheduler-query: authoritative aggregate (REMOTE-012) op=fleet.aggregate status=ok as_of=2026-11-07T10:40:00Z
  peer=peer-a status=ok stale=false last_contact=2026-11-07T10:39:58Z
  peer=peer-b status=error stale=true last_contact=2026-11-07T09:12:00Z code=timeout
{ …the merged §2.2 envelope, indented… }
```

`--all --json` prints that merged envelope as one compact line. `--legacy-all`
renders one summary + envelope block **per registered peer**, in registry order.

### Degradation vocabulary

A peer that produced no answer is rendered as a `status="error"` envelope with
one of these codes (`query.go:434-453`) — never dropped, never called "down":

| Code | Cause |
|------|-------|
| `timeout` | the peer did not answer inside `budget_ms` |
| `reply_closed` | the reply stream closed without a matching frame |
| `bus_disabled` | the transport is disabled |
| `transport_error` | any other transport failure (dial refused, relay refusal, publish rejected) |

The human summary adds `last_contact=` from the local registry, with the
no-reason law applied: a timestamp when a heartbeat exists, `never` for a
registered peer that never checked in, `unknown` when no registry could be read
(`query.go:537-545`).

### Exit codes

| Code | Meaning | Triggers |
|------|---------|----------|
| `0` | every answer ok | one ok envelope; an ok `--all` aggregate; `--legacy-all` with every leg ok; `--help` |
| `1` | degraded — an answer was produced but not all of it is ok | any non-ok status in single-peer mode, any non-ok leg in `--legacy-all`, an aggregate whose status is not `ok` |
| `2` | hard error — the query could not be attempted | positional/`--want`/`--arg`/unknown-flag errors; `--legacy-all` with a missing, unreadable or empty registry; `--all` unreachable, refused (`401`/`403`/`503`), non-200, or not a §2.2 envelope |

Measured probes on the built binary:

```
$ /tmp/scheduler-query --help                                  → usage block on stderr, rc=0
$ /tmp/scheduler-query                                         → "expected: scheduler-query <peer> <op> [flags]  (or --all <op>)" + usage, rc=2
$ /tmp/scheduler-query --bogus                                 → "flag provided but not defined: -bogus" + usage, rc=2
$ /tmp/scheduler-query --want bogus peer-a peer.status         → '--want must be "answer" or "partial"', rc=2
$ /tmp/scheduler-query --arg noequals peer-a peer.status       → 'invalid value "noequals" for flag -arg: --arg must be k=v (got "noequals")', rc=2
$ /tmp/scheduler-query peer-a peer.status --url http://127.0.0.1:1
      → status=error code=transport_error last_contact=unknown, rc=1
$ /tmp/scheduler-query --all fleet.status --daemon-url=http://127.0.0.1:1
      → "aggregate at … unreachable: … connection refused — start the daemon or pass --daemon-url", rc=2
$ /tmp/scheduler-query --legacy-all fleet.status --db /tmp/absent.db
      → "no peer registry at /tmp/absent.db — --all reads the local registry; register peers or target one peer", rc=2, file NOT created
```

Because a degraded answer is exit 1 and a peer refusal is also exit 1, read the
rendered envelope (or `--json`'s `status`), not the exit code alone, to learn
*which* peer failed and why.

### Where the help text and the code disagree

1. **`--daemon-url URL` only works with `=`.** The positional hoister mirrors
   the flag package's value consumption from a hand-kept map
   (`main.go:199-202`) that lists `want`, `budget-ms`, `corr-id`, `url`, `db`
   and `arg` — but **not** `daemon-url`. With the space-separated form the URL
   is hoisted into the positional list and the flag is left value-less.
   Measured: `--all fleet.status --daemon-url http://127.0.0.1:1` →
   `flag needs an argument: -daemon-url`, rc=2; with `--db` also present →
   `--all takes exactly one positional: <op>`, rc=2. Use `--daemon-url=URL`.
   Every other value-taking flag accepts both forms.
2. **`-h`/`--help` is not a registered flag**; it is the `flag` package's
   built-in help path. Here that path exits **0**, while a usage error exits 2,
   and the block goes to **stderr** (`main.go:98`, `help.go:83-85`) — capture
   with `2>&1` before grepping it.
3. **The "no relay URL" hard-error branch is unreachable**
   (`main.go:156-159`): `--url`, then `CRIER_URL`, then
   `http://127.0.0.1:8767` always leave a non-empty value, so the help's
   exit-2 case "no relay URL" cannot be produced.
4. The help's exit-code note says `1` means "at least one peer answered
   stale/partial/error"; a single-peer query whose only peer errored also exits
   1 (`query.go:169-172`).
5. `--json` is described as "the raw §2.2 response envelope(s) only". That is
   the `--json` shape; the default human shape prints a summary line **and** the
   envelope, pretty-printed.
6. The `--legacy-all <op>` line in the usage stanza omits `[flags]` even though
   every flag applies to it.
7. A negative `--budget-ms` is silently treated as unset: the envelope omits
   `budget_ms` and the local ceilings fall back to 10 000 ms
   (`query.go:299-302,196-200,348-351`).
8. A minted `--corr-id` is unique per *invocation*, not across a process life:
   the counter restarts at 1 in every new process, so the trailing field reads
   `…-0000000000000001` for the first query of each run
   (`internal/bus/query.go:171-174`).
9. With both `--all` and `--legacy-all`, `--all` silently wins — no error and no
   warning (`main.go:104-113`).
10. The `--legacy-all` "no peer registry" message says "`--all` reads the local
    registry" (`query.go:508`); it is printed for `--legacy-all`, which is the
    mode that actually reads the local registry.

### Credentials

- No credential flag exists **on purpose**: `CRIER_AUTH_TOKEN` (relay) and
  `SCHEDULER_OPERATOR_TOKEN` (daemon gate) are read from the environment only,
  so tokens never appear in argv, `ps` output or shell history.
- `CRIER_AUTH_TOKEN` → `Authorization: Bearer …` on the relay publish and
  subscribe; `SCHEDULER_OPERATOR_TOKEN` → `X-Operator-Token` on `--all`. Both
  are optional for a loopback, ungated deployment.
- This page records variable **names** only; no credential value is written
  here, and none should be pasted into a shell line.
- Database access is read-only in practice: single-peer mode only reads
  last-contact values and `--legacy-all` refuses a missing registry instead of
  creating one, so no run of this tool initializes a database.

## `cmd/scheduler-dispatch`

`scheduler-dispatch` is the operator surface of the DISPATCH verb
(SCHED-GAP-1665; [../dispatch-spec.md](../dispatch-spec.md)). It hands ONE unit
of work to ONE named agent over the Crier relay's durable per-agent inbox. It is
a thin adapter too: it parses its surface, builds a `bus.WorkItem`, and hands it
to `bus.Client.Dispatch` — it resolves no target, looks up no lane and retries
nothing, because those belong to the scheduler.

**It never names a task.** The envelope carries the lane the scheduler picked, a
board/workdir reference the agent resolves against its own checkout, and a
correlation id: the scheduler picks the LANE, the foreman picks the TASK. That
boundary is the reason the verb exists (`internal/bus/dispatch.go:1-58`).

Sources: `cmd/scheduler-dispatch/main.go` (the whole surface), with the wire
shape and validation in `internal/bus/dispatch.go` (`validate` 146-168,
`payload` 175-214, `BuildDispatch` 266-290, `DispatchTo` 321-391).

### Invocation

```bash
# The help probe — reproducible in a fresh checkout, touches nothing.
# The block goes to STDERR and the process exits 2 (help is a usage outcome
# here: discrepancy 1).
go run ./cmd/scheduler-dispatch --help

go build -o /tmp/scheduler-dispatch ./cmd/scheduler-dispatch/

# 1. Evidence before action: render the endpoint and the exact body, send nothing.
/tmp/scheduler-dispatch auger --lane docs --board /srv/fleet/tasks.jsonl \
  --text-file /tmp/directive.txt --dry-run

# 2. The real hand-out (same flags, minus --dry-run).
/tmp/scheduler-dispatch auger --lane docs --board /srv/fleet/tasks.jsonl \
  --text-file /tmp/directive.txt --corr-id docs-2026-11-07-1
```

The usage stanza of that probe (measured; exit status **2**), with the
host-derived default in `--scheduler-id`:

```
usage: scheduler-dispatch <agent-id> --lane <lane> (--board <ref> | --workdir <ref>) [flags]

Hand ONE unit of work to ONE named agent over the Crier relay's durable
inbox. The scheduler picks the LANE; the foreman picks the TASK — the
payload never names a task.

flags:
  --lane <lane>          lane whose work this is (required)
  --board <ref>          board reference the agent resolves (path, or path@host)
  --workdir <ref>        workdir reference the agent resolves
  --corr-id <id>         correlation id (default: minted unique; reused as the
                         relay's request id + idempotency key, so retrying the
                         SAME id is safe)
  --text <directive>     what the node's harness is asked to carry. Without it
                         the payload has no directive member at all and the
                         node receives the bare envelope, so an operator
                         dispatch carries its own prompt here.
  --text-file <path>     read the directive from a file (same member; mutually
                         exclusive with --text — quoting a long prompt in argv
                         is how a shell mangles it)
  --url <url>            Crier relay base URL (env CRIER_URL; default http://127.0.0.1:8767)
  --scheduler-id <id>    sender identity (env SCHEDULER_ID; default karaHermes-mde-7840hs)
  --dry-run              print the endpoint + exact body, send nothing
  --json                 print the receipt as JSON

env: CRIER_URL, CRIER_AUTH_TOKEN (the credential — never a flag), SCHEDULER_ID

exit: 0 accepted; 1 not accepted (refused / unknown target / unreachable); 2 usage
```

### Positional

| Position | Rule |
|----------|------|
| `<agent-id>` | exactly one, required — the registered agent the work is addressed to (`main.go:131-140`) |

Zero or more than one positional prints the usage block and exits **2**
(`main.go:136-139`). The id is trimmed; a whitespace-only id passes the count
check and is then refused by `BuildDispatch` as `bus: dispatch refused: agent id
is required` (exit 2). Positionals may appear anywhere among the flags
(`splitPositionals`, `main.go:246-276`), and `--` ends flag parsing.

### Flags

| Flag | Type | Default | Effect | Source |
|------|------|---------|--------|--------|
| `--lane <lane>` | string | `""` | lane whose work this is; **required** (validated in the bus, not the flag surface) | `main.go:119`, `dispatch.go:147-149` |
| `--board <ref>` | string | `""` | a reference the agent resolves (path, or `path@host`); one of board/workdir is required | `main.go:120`, `dispatch.go:150-152` |
| `--workdir <ref>` | string | `""` | a workdir reference the agent resolves; one of board/workdir is required | `main.go:121` |
| `--corr-id <id>` | string | `""` → minted | correlation id; also the relay `request_id` and `idempotency_key`, so retrying the same id is safe | `main.go:122`, `dispatch.go:274-285` |
| `--text <directive>` | string | `""` | inline directive; sent base64 as `payload.text_b64` | `main.go:123`, `dispatch.go:196-203` |
| `--text-file <path>` | string | `""` | read the directive from a file; mutually exclusive with `--text` | `main.go:124,203-221` |
| `--url <url>` | string | `""` → `CRIER_URL` → `http://127.0.0.1:8767` | relay base URL | `main.go:125,142` |
| `--scheduler-id <id>` | string | `""` → `SCHEDULER_ID` → short hostname → `scheduler` | sender identity (`payload.scheduler_id`, `sender`) | `main.go:126,63-76,143` |
| `--dry-run` | bool | `false` | render the endpoint and exact body, open no connection | `main.go:127,168-171` |
| `--json` | bool | `false` | print the accept receipt as indented JSON | `main.go:128,182-187` |

All value-taking flags are mirrored in the hoister's map
(`main.go:247-256`), so `--flag V` and `--flag=V` both work here — including
`--text` and `--text-file`, whose omission from that map would have turned their
argument into a positional and produced a phantom usage error (`main.go:250-254`).

### Environment

| Variable | Read at | Effect |
|----------|---------|--------|
| `CRIER_URL` | `main.go:54,142` | relay base URL; overridden by `--url` |
| `CRIER_AUTH_TOKEN` | `main.go:144` | relay credential, sent as `Authorization: Bearer …` **only when non-empty** |
| `SCHEDULER_ID` | `main.go:64` | sender identity; overridden by `--scheduler-id`, else the short hostname, else the literal `scheduler` |

No other variable is read (`grep os.Getenv cmd/scheduler-dispatch/*.go` finds only
`envOr`'s keys).

### Validation — all of it happens before I/O, all of it exits 2

| Rule | Exact message |
|------|---------------|
| lane required | `bus: dispatch: lane is required` |
| board **or** workdir required | `bus: dispatch: a board or workdir reference is required` |
| directive must not parse as JSON | `bus: dispatch: text is a JSON blob, not a directive` |
| `--text` + `--text-file` together | `--text and --text-file are mutually exclusive` |
| `--text-file` unreadable | `--text-file <path>: open <path>: no such file or directory` |
| `--text-file` empty | `--text-file <path> is empty` |
| positional count ≠ 1 | the usage block |

The first three come from `bus.WorkItem.validate` (`dispatch.go:146-168`), so
they print a single `scheduler-dispatch: …` line and exit 2 **without** the
usage block. The JSON refusal exists because a harness handed a JSON blob is
asked to execute an envelope rather than a task (CF-139): the test is "parses as
JSON", not "starts with a bracket", so a real directive opening with
`[Scheduler tick: …]` still passes.

### The `--dry-run` contract

Measured with `--lane docs --board /tmp/board.jsonl`:

```
DRY RUN (nothing sent)
  POST http://127.0.0.1:8767/agents/auger/inbox
  corr_id: q-karaHermes-mde-7840hs-1794047773390669368-0000000000000001
  body: {"payload":{"board":"/tmp/board.jsonl","corr_id":"q-karaHermes-mde-7840hs-1794047773390669368-0000000000000001","issued_at":"2026-11-07T10:36:13.390669368Z","kind":"work.dispatch","lane":"docs","scheduler_id":"karaHermes-mde-7840hs"},"sender":"scheduler-karaHermes-mde-7840hs","request_id":"q-karaHermes-mde-7840hs-1794047773390669368-0000000000000001","idempotency_key":"q-karaHermes-mde-7840hs-1794047773390669368-0000000000000001"}
```

- It performs **no** HTTP request (a stub relay on `--url` records zero
  requests), opens no socket and exits **0**.
- The body is rendered by the same `bus.BuildDispatch` call the live path uses,
  so the member set and bytes match that invocation exactly.
- The correlation id and `issued_at` are minted **per invocation**: to send the
  very body you inspected, pass the printed id back as `--corr-id <that id>`.

### What reaches the wire

`POST <url>/agents/<agent-id>/inbox`, JSON body:

| Member | Presence | Value |
|--------|----------|-------|
| `payload.kind` | always | `work.dispatch` (`dispatch.go:63`) |
| `payload.lane` | always | `--lane` |
| `payload.corr_id` | always | `--corr-id`, else minted `q-<scheduler-id>-<unixnano>-<016 counter>` |
| `payload.scheduler_id` | always | resolved sender id |
| `payload.issued_at` | always | RFC3339Nano, UTC |
| `payload.board` | only when `--board` is set | the board reference |
| `payload.workdir` | only when `--workdir` is set | the workdir reference |
| `payload.text_b64` | only when a directive resolved | **base64** (std encoding) of the directive — never plain `text` (`dispatch.go:196-203`) |
| `payload.reply_to` | **never from this CLI** | no flag sets `WorkItem.ReplyTo` |
| `payload.limits.wall_s` | **never from this CLI** | no `--wall-s` flag |
| `sender` | always | `scheduler-<scheduler-id>` |
| `request_id` | always | same value as `payload.corr_id` |
| `idempotency_key` | always | same value as `payload.corr_id` |

The directive is base64 on purpose: the relay's prompt-injection pre-match
treats an escaped-quote JSON fragment inside a string as a stringified object
and refuses the whole dispatch `403 GUARD_BLOCKED`, and real foreman prompts
quote JSON. The receiver decodes `text_b64` before the harness sees it
(`dispatch.go:196-203`).

Because `idempotency_key` is the correlation id, re-running the identical command
with the same `--corr-id` is one message, not two: the relay answers from the
recorded accept and reports `idempotent_replay: true`.

### Output

Human mode, on accept (`main.go:188-194`):

```
DISPATCHED agent=auger lane=docs corr=doc26-corr-1 transport=inbox message=msg_01HSTUB replay=false
```

`transport` is the relay's own value (`inbox`, `webhook`, `held`), or `accepted`
when the relay names none; `message=-` when the accept carries no id (measured
against a stub answering `202 {"transport":"held","status":"held"}` → `…
transport=held message=- replay=false`); `replay=true` marks an
idempotency-key replay.

`--json` prints the accept receipt as indented JSON — the field names are Go
struct fields, not snake_case (`bus.DispatchReceipt`, `dispatch.go:236-256`).
Measured:

```
{
  "AgentID": "auger",
  "CorrID": "doc26-corr-1",
  "MessageID": "msg_01HSTUB",
  "Transport": "inbox",
  "IdempotentReplay": false,
  "Status": "queued"
}
```

A receipt is printed **only** on an accept. A failure always prints
`scheduler-dispatch: NOT dispatched: <err>` on stderr and nothing on stdout,
`--json` or not (measured with an unreachable relay:
`scheduler-dispatch: NOT dispatched: bus: dispatch agent "auger" (corr …): bus:
dispatch unreachable: Post "http://127.0.0.1:1/agents/auger/inbox": dial tcp
127.0.0.1:1: connect: connection refused`, rc=1).

### Exit codes

| Code | Meaning | Triggers |
|------|---------|----------|
| `0` | the relay **accepted** the delivery | any 2xx accept (`201` inbox, `200` blocking webhook, `202` async/held), including an idempotent replay; `--dry-run` |
| `1` | **not** accepted — the job was not handed out | relay `404` (unknown target, `bus: dispatch target unknown`), any other 4xx (`bus: dispatch refused`), any 5xx (`bus: dispatch unreachable`), or a dial/timeout failure |
| `2` | usage or configuration error — nothing was attempted | positional count, unknown flag, missing `--lane`, missing board/workdir, JSON-looking directive, `--text` + `--text-file`, unreadable or empty `--text-file` |

Exit `0` means *accepted by the relay*, not *executed by the agent*: the
dispatcher's own run is a separate fact (`docs/dispatch-spec.md`).

Measured probes on the built binary (accept/refusal cases against local stubs):

```
$ /tmp/scheduler-dispatch --help                                   → usage block on stderr, rc=2
$ /tmp/scheduler-dispatch                                          → usage block, rc=2
$ /tmp/scheduler-dispatch auger extra --lane docs --board b        → usage block, rc=2
$ /tmp/scheduler-dispatch --bogus auger --lane docs --board b      → "flag provided but not defined: -bogus" + usage, rc=2
$ /tmp/scheduler-dispatch auger --workdir /tmp/x --dry-run         → "bus: dispatch: lane is required", rc=2
$ /tmp/scheduler-dispatch auger --lane docs --dry-run              → "a board or workdir reference is required", rc=2
$ /tmp/scheduler-dispatch '   ' --lane docs --board b --dry-run    → "bus: dispatch refused: agent id is required", rc=2
$ /tmp/scheduler-dispatch auger --lane docs --board b --text '{"x":1}' --dry-run
                                                                   → "text is a JSON blob, not a directive", rc=2
$ /tmp/scheduler-dispatch auger --lane docs --board b --text a --text-file /etc/hostname --dry-run
                                                                   → "--text and --text-file are mutually exclusive", rc=2
$ /tmp/scheduler-dispatch auger --lane docs --board b --text-file /tmp/empty.txt --dry-run
                                                                   → "--text-file /tmp/empty.txt is empty", rc=2
$ /tmp/scheduler-dispatch auger --lane docs --board b --url http://127.0.0.1:1
                                                                   → "NOT dispatched: … dispatch unreachable: … connection refused", rc=1
$ /tmp/scheduler-dispatch auger --lane docs --board b --url <relay answering 404>
                                                                   → "NOT dispatched: … dispatch target unknown: unknown agent", rc=1
$ /tmp/scheduler-dispatch auger --lane docs --board b --url <relay answering 500>
                                                                   → "NOT dispatched: … dispatch unreachable (500): relay exploded", rc=1
$ /tmp/scheduler-dispatch auger --lane docs --board b --url <relay accepting>
                                                                   → "DISPATCHED agent=auger lane=docs corr=… transport=inbox message=msg_01HSTUB replay=false", rc=0
```

### Where the help text and the code disagree

1. **`--help` exits 2, not 0.** The flag set is built with `ContinueOnError` and
   `-h` is not defined, so `fs.Parse` returns `flag.ErrHelp` and `run` maps every
   parse error — help included — to 2 (`main.go:117,132-134`). The block printed
   is the same one a usage error prints. `go run ./cmd/scheduler-dispatch --help`
   also appends `exit status 2` to stderr and itself exits 1 (`go run`'s own
   wrapper), so measure the exit code on the built binary.
2. **The help block is host-specific.** `--scheduler-id`'s default is resolved
   and interpolated into the text at print time (`main.go:100,107`), so on this
   host it reads `default karaHermes-mde-7840hs`. Do not diff the block across
   hosts, and do not treat the printed hostname as a configured value.
3. **Both `--board` and `--workdir` may be supplied**, although the usage line
   shows `(--board <ref> | --workdir <ref>)`. `validate` requires *at least*
   one, and the payload then carries both (measured; `dispatch.go:146-168,
   175-214`).
4. **Half the validation prints no usage block.** A missing `--lane`, a missing
   board/workdir reference, or a JSON-looking `--text` produces a single
   `scheduler-dispatch: …` line and exit 2, because those rules live in the bus
   library rather than the flag surface (`main.go:163-167`).
5. **An empty `--text-file` is refused** (`is empty`) even though an absent
   directive is legal. The help's "Without it the payload has no directive
   member at all" describes omitting both directive flags, not pointing
   `--text-file` at an empty file.
6. **`--json` only changes the success path.** A failure prints the human error
   line on stderr and nothing on stdout.
7. **No `--wall-s` and no `--reply-to` flag.** A dispatch from this CLI therefore
   never carries `payload.limits.wall_s` or `payload.reply_to`; the scheduler's
   internal dispatch path sets both, which is why the fields exist on
   `bus.WorkItem` (`dispatch.go:124,141`).
8. **`--dry-run`'s body is byte-identical for that invocation only** — the
   correlation id and `issued_at` are minted per run, so a later real run differs
   unless you pass the printed id back via `--corr-id`.
9. The `env:` line of the help lists the three variables correctly, but it is
   the only place `SCHEDULER_ID`'s host fallback (`short hostname`, then the
   literal `scheduler`) is not spelled out — the code path is
   `main.go:63-76`.

### Credentials

- `CRIER_AUTH_TOKEN` is the only credential. It is read from the environment and
  sent as `Authorization: Bearer …` **only when non-empty** (measured against a
  stub relay with the variable unset: no `Authorization` header arrives at all).
- There is deliberately **no** `--token` flag: credentials in argv leak through
  `ps` (GAP-038, `internal/bus/dispatch.go:24-26`).
- This page records variable **names** only; no credential value is written
  here.
