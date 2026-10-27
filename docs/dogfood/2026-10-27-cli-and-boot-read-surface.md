# Dogfood 2026-10-27 — the operator command line, and a current-HEAD restart

- **Tick:** `coding-hermes-scheduler-dogfood-2026-10-27-15-09-28` (dogfood lane, read-only on the product)
- **Product HEAD:** `889497d7b23bdaed34152e5156cac9fa59da376c` (detached worktree of HEAD, clean checkout; primary tree head of the same sha)
- **Build under test:** `make build` 2.70s wall → `bin/schedulerd` / `bin/migrate` **v1.12.0-668-g889497d7** (build_time 2026-10-27T15:16:37Z); `/api/v1/health` reports `build_sha=889497d7`
- **Live daemon (read-only reference):** build `8ec55a6f` (v1.12.0-566), started 2026-10-22T08:14:06-05:00, uptime 122h, `127.0.0.1:9090`
- **Angle chosen (uncovered by prior runs):** the operator command line — `cmd/scheduler-query` (the federation read client) and `bin/migrate` (the documented cron importer) — neither of which any dogfood run has ever driven (0 occurrences of either binary name in the run history), plus a **restart of a current-HEAD daemon against a copy of the live database**.

## 1. Promise statements driven

| Source | Promise |
|---|---|
| `README.md:60-190` (Getting Started §4-§6) | `make build` yields `bin/schedulerd` + `bin/migrate`; `make migrate-dry` previews; ineligible jobs are skipped with a per-job `SKIP <name>: …` reason; a boot prints its DB path, project count and `schedulerd ready`. |
| `README.md:88` | The prompt must contain a workdir path, e.g. `` `Workdir: /home/...` `` **or** `` `workdir /home/...` ``. |
| `README.md:384,1085-1112` + `docs/federation-query-spec.md` | `scheduler-query <peer> <op>` reads one peer; the aggregate (`--all`) delegates to the daemon's `fleet.aggregate`; allow-lists are per caller identity, deny-all until configured, filled with "§2.3 op ids". |
| `cmd/scheduler-query --help` (self-documenting) | `scheduler-query --all <op> [flags]` is "the AUTHORITATIVE aggregate (REMOTE-012)"; `--daemon-url URL  scheduler daemon base URL for --all`. |

## 2. What worked (verified, no row filed)

- **`bin/migrate` diagnostics hold.** On the box's real `jobs.json` (158 jobs): `Done: 0 imported, 158 skipped`, one `SKIP <name>: not a coding-hermes job (name/skills must mention coding-hermes/foreman)` per job. On a 3-job fixture: `alpha` imported, the other two skipped **with reasons**. `DOGFOOD-017`'s old defect (silent skips) stays fixed.
- **`--dry-run` really is a dry run.** sha256 of the DB copy identical before/after (`2cdbd06ea5b238f8`); output ends with `DRY-RUN (nothing written; pass --apply to write)`-equivalent wording and `Run without --dry-run to perform the import.`
- **Import is idempotent** (`SKIP alpha: already exists`; project count unchanged at 1) and lands the documented defaults (`enabled=1, w=10, p=5, cooldown_s=900` — the API create path's 900s default, not the config loader's 7200s).
- **`scheduler-query` error handling is honest** where it is allowed to be: unknown aggregate op → HTTP 400 with `error.code=unknown_op` naming the six supported ops; unknown peer → named `status=error code=transport_error`; `--all` with no op → `--all takes exactly one positional: <op>` plus usage; `--json` → one compact envelope per line.
- **Nothing was slow enough for a user to feel.** `--all fleet.status` warm mean **10 ms** (n=20 samples: 8, 8, 9, 9, 9, 10, 10, 10, 10, 11, 26, 29, 8, 9, 10, 8, 8, 9, 10, 9); `--help` 7-10 ms; `migrate --dry-run` 0.01-0.02 s; daemon boot-to-ready <0.5 s on an empty DB; `/api/v1/config` 0.36 ms. **No PERF row is warranted for any of these** — a win nobody can feel is not a finding.

## 3. What broke

### 3.1 PERF-001-R3-R3 (P1, recurrence) — the boot evaluate pass still starves every DB-backed read

Two boots of the HEAD build against a byte copy of the live DB (`/tmp/df-cli-1027/live-copy.db`, 8,434,626,560 bytes; 93,731 ticks / 845 projects / 822,427 events), `--min-interval 24h --max-concurrent 1 --no-exec-fallback`:

```
t+1s   /api/v1/health   504 @1.000972s  (bytes=95)
t+2s   /api/v1/status   504 @5.000680s  (bytes=91)
t+7s   /api/v1/health   504 @1.000641s
t+8s   /api/v1/status   504 @5.001014s
t+15s  /api/v1/health   504 @1.000520s
t+16s  /api/v1/status   504 @5.000567s
t+30s  /api/v1/health   504 @1.001261s
t+31s  /api/v1/status   504 @5.001462s
t+60s  /api/v1/health   504 @1.001113s
t+61s  /api/v1/status   504 @5.000767s
t+90s  /api/v1/health   504 @1.000402s
t+91s  /api/v1/status   504 @5.000901s
       /                 200 @2.502585s  (bytes=40320 — live daemon serves 142,115 B)
```
First boot: the same 504s at t+0…t+50 s, and recovery by ≈t+2.5 min (`/health` 200 @0.137 s, `/status` 200 @0.4498 s / 70,125 B).

The daemon's own log names the steps: `WARN: slow request handler=health step=countActiveTicks elapsed=1s … budget=1s`, `SLOW-REQ: GET /api/v1/status 5s status=504 bytes=91 wait=not-instrumented`, same for `projects` (`ListProjectsPage`) and `cadence` (`ListProjects`). Every response returns at exactly its budget → the handlers **wait**.

Goroutine dump during the window (`/debug/pprof/goroutine?debug=1`, 104 lines) puts the holder in the evaluator:

```
Loop.Run loop.go:884 → Loop.evaluate tick_process.go:317 → Packer.Pick packer.go:336
 → PendingTaskCounter.CountPending board_awareness.go:276
 → freshnessCheckedPending board_awareness.go:313 → cachedFreshnessRead board_awareness.go:184
 → FreshnessVerdictCache.Read board_freshness_cache.go:163 → readFull board_freshness_cache.go:204
 → ReadBoardFreshness board_freshness.go:613 → classifyCompleteRow board_freshness.go:683
 → gitRevVerifyCommit board_freshness.go:886   (fork/exec)
```

Controls that exclude the host and the disk: the **live daemon on the same box in the same minutes** answered `/status` 200 @0.3447-0.3678 s (n=5, 70,697 B) and `/` 200 @0.0052 s warm; read-only `sqlite3` counts on the same copy returned `93731|845|822427` in **0.17 s**; loadavg 5.9-6.9 on 16 cores; `/api/v1/config` (no DB access) answered 200 @0.36 ms throughout the window.

**Confound, disclosed:** the DB is a copy, not the live file (it was never written to), so the boot evaluation re-derives state on a snapshot. The shape is what a real restart exercises. Classification: **recurrence on a current-HEAD build** (the deployment-binary rule is satisfied), explicit precondition of the criterion met.

### 3.2 SCHED-GAP-16930 (P2) — the documented aggregate is un-enableable from the docs

`scheduler-query --all <op>` posts `op=fleet.aggregate`. On a scratch daemon:

| Arm | Result |
|---|---|
| valid token, no allow config | HTTP 403, `error.code=op_not_allowed`, `"op fleet.aggregate is not published for caller operator:token (spec §4 … see [federation] allow config)"` |
| same build, `fleet.aggregate` added to `[federation.allow."operator:token"]` | boot line `FEDERATION: read policy armed (1 caller grant(s))` → `status=ok, peer_count=1, answered=1, degraded=0`, `self:true` row |

Yet `GET /api/v1/federation/catalogue` serves `{"count":6, "ops":[peer.status, fleet.status, projects.list, queue.get, ticks.list, events.list]}` and `README.md:1085-1112` instructs operators to fill the allow list with "the `docs/federation-query-spec.md` §2.3 op ids" — the six catalogue ops. `grep -rn fleet.aggregate docs/ README.md cmd/` finds the op only in `docs/api.md:1982-2005` and the CLI/tests. Live fleet: `/api/v1/config` → `federation {deny_all:true, allow_count:0, callers:[]}`, so `--all` is refused there too.

### 3.3 SCHED-GAP-16931 (P3) — the refusal advisory names the wrong remedy

403 `op_not_allowed` (credential valid and presented) is answered with `… refused (HTTP 403) — set SCHEDULER_OPERATOR_TOKEN`. 401 and 403 share one advisory, so the operator is told to do the thing they already did. Discriminating probes (garbage values only): no env → `operator credential required for mutations`; `SCHEDULER_OPERATOR_TOKEN=<garbage>` → `invalid operator credential`; `CRIER_AUTH_TOKEN=<garbage>` → unchanged ⇒ the `--all` path reads `SCHEDULER_OPERATOR_TOKEN`, while `docs/reference/env-vars.md:148` states the CLIs take the credential from `CRIER_AUTH_TOKEN`.

### 3.4 SCHED-GAP-16932 (P3) — the CLI does not parse its own usage line

| Invocation | Result |
|---|---|
| `--all peer.status --daemon-url http://…` (the documented `[flags]` position) | `flag needs an argument: -daemon-url` + help dump, rc 2 |
| `--all peer.status --daemon-url=http://…` | works |
| `--daemon-url http://… --all peer.status` | `--all` is consumed as the flag's **value**; the URL becomes the `<peer>` positional → silent single-peer mode, `status=error code=transport_error`, no error |
| `--all projects.list --arg filter=x --daemon-url <url>` | same failure class when space-separated after the op |

### 3.5 README-20 (P4) — a documented prompt form the importer rejects

`README.md:88` accepts `` `Workdir: /home/...` `` **or** `` `workdir /home/...` ``; `cmd/migrate/main.go:37` is `regexp.MustCompile(`[Ww]orkdir:\s*(\S+)`)` — colon required. Measured on a 6-job fixture:

| prompt form | result |
|---|---|
| `workdir /tmp/…/realdir` | **SKIP — no workdir found in prompt** (the README's own example) |
| `Workdir: /tmp/…/realdir` | imported |
| `workdir: /tmp/…/realdir` | imported |
| `Workdir /tmp/…/realdir` | **SKIP** |
| `Workdir: /tmp/…/absentdir` | imported (no existence check) |
| `Do a tick with Workdir: /tmp/…/realdir inline` | imported |

Same leg, for `DOC-28`'s ambit: the imported project **name is the workdir basename** (`cmd/migrate/main.go:197-198`), not the job name — three jobs pointing at one directory collapse into one project (`realdir`), which the README never states.

## 4. Install leg

Recorded as **PARTIAL** (not SKIPPED): `bunker-qa.sh launch /home/kara/coding-hermes-scheduler/coding-herms-scheduler --keep` → `agent=77b3809d @ bunker-mvp`, launched 2026-10-27T15:34:34Z, TTL 4h, `QA-HEAD=889497d7…` (clone dirty → pull skipped, testing the recorded sha), fence check `body_lines=2276 intended_substitutions=35 violations=0`, cells `fresh-install` + `upgrade-prep` started. Evidence target: `/tmp/bunker-qa-evidence-20261027T153434Z-180555.jsonl`. The battery needs 7-15 min; the next tick owes `bunker-qa.sh collect --evidence <that path>`, the per-cell grades, and the teardown proof.

Install-**equivalent** proof, completed inside this tick: clean detached worktree of HEAD → `make build` 2.70 s → zero-config boot (`/tmp/df-cli-1027/sched2.db`, `:19092`) ready in one readiness poll (<0.5 s) with `/api/v1/health` 200, an honest fail-closed banner (`Auth: FAIL-CLOSED — no operator credential configured`), `FEDERATION: read policy DENY-ALL`, and `CRIER: bus disabled (no-op)`.

## 5. Footprint

Read-only on the product and the fleet: no source, no scheduler state, no fleet config, no repo visibility and no credential touched; all live-daemon traffic was GET; the only credential presented was one minted for the scratch instance. Scratch: `/tmp/df-cli-1027/` (`src/` = detached worktree of HEAD, `ev/`, `live-copy.db`, `fed.toml`, `synth*.json`), with evidence copies under `~/.hermes/stand-in/dogfood/coding-hermes-scheduler/scratch/2026-10-27/`.

Observed state, **not filed** (two identical samples 4 minutes apart): the primary checkout is diverged from `origin/main` — ahead 2 (`051d6ce0`, `889497d7` board commits) and behind 4 (`3d0bbcee`, `cbc819af`, `9e7b3b25`, …), tree clean.
