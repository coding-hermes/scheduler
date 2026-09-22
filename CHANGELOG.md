# Changelog

All notable changes to the Coding Hermes Scheduler.

## How to read this file

Each release heading carries its **tag** and the **commit range** its body was
derived from, so any line here can be checked against the repository:

```sh
git log --oneline v1.2.0..v1.3.0        # the range cited under the heading
git rev-parse v1.3.0^{commit}           # every cited tag resolves to a commit
```

Every tag in this file is an **annotated** tag object (`git cat-file -t v1.3.0`
→ `tag`).

**Level of detail.** The `v1.0.0..v1.1.0` range carries **1117 commits** (427
board rows, 349 chore/bookkeeping, 57 foreman ticks — the fleet's own
operational traffic). That range is summarised *by category with the row ids
named* and the range cited, rather than listing every commit; the smaller ranges
(1/167/36 commits) are listed at commit-subject level. Routine bookkeeping
(`board:` rows, `foreman tick`, `chore(gitreins)`, `Merge`) is never itemised —
only counted.

**Scope note.** The entries for v1.1.0, v1.1.1, v1.2.0, and v1.3.0 were
reconstructed on 2026-09-20 from real commits and real tags. This file had been
frozen at 2026-08-04 (`167cc99d`) while those four releases shipped; their
GitHub Release notes existed only on the Release objects, off-repo and outside
review. Nothing below is invented — every line traces to a commit subject or a
range cited under its heading. The rollover itself is done by
`make release-prep VERSION=vX.Y.Z`; see `docs/releases.md`.

## [Unreleased] — 2026-08-04

Entries accumulated here after `1.0.0`. **This section shipped as part of
v1.1.0** — it was written by `167cc99d`, which is inside the `v1.0.0..v1.1.0`
range — but it was never given a release heading of its own.

### API Conformance (DOGFOOD-001/003/004)

- **snake_case wire format** — `Project`, `Tick`, `Event`, `ProjectUpdates` now carry S02/S06 `json` tags; responses emit `name`, `repo_url`, `cooldown_s`, `active_projects`, etc. per the OpenAPI contract
- **Backward-compatible request decoding** — custom `UnmarshalJSON` on `Project` and `ProjectUpdates` accepts snake_case AND legacy PascalCase keys (`Name`, `CooldownS`, `Enabled`, …); live fleet automation (fleet-auto-heal, stand-in gap-pusher) is unaffected
- **Create defaults** — `POST /api/v1/projects` fills `weight=10`, `priority=5`, `cooldown_s=900`, `decay_rate=1.0` when omitted; the documented minimal body `{name, repo_url, workdir}` now works; new projects stay disabled
- **Error mapping** — SQLite CHECK-constraint violations return `400` with an actionable range message (previously `500`); duplicate name/workdir still return `409`
- **Conformance tests** — `internal/api/conformance_test.go`: 11 tests covering both request spellings, 400/409 mapping, and exact response field names for `/projects`, `/status`, `/ticks`, `/events`
- **Spec S06 approved** — §1.1 conformance section rewritten to match verified behavior; status flipped Draft → Approved
- **README** — fixed `jq '.project_count'` → `jq '.active_projects'`; added "API wire format" note

### Verification Harness (DOGFOOD-002)

- **Fixed `--test-verify` red streak (50+ runs since 2026-07-31)** — fixture projects now get unique workdirs (`tmpDir/<name>`, created via `os.MkdirAll`) so the case-insensitive dup-workdir guard no longer rejects "beta"
- **Priority-ordering check fixed** — the per-eval spawn-order assertion was unsatisfiable: slot-pool goroutines start concurrently, so sub-second `spawned_at` order is scheduler-shuffled, not pack order. The check now asserts first-cycle set membership against the expected greedy-knapsack pack (urgency/priority ordering is still enforced)
- **CI gate** — `.github/workflows/ci.yml` build job now runs `./bin/schedulerd --test-verify 3` after "Build binaries" so the end-to-end verify can't silently rot again

### Observability — commit anatomy (SCHED-PERF-001-B)

- **`cmd/backfill-commit-signals`** — reconstructs the code/board commit split for ticks whose `code_commits`/`board_commits` were never stamped (dry-run by default, `--apply` to write; unmeasurable ticks get the explicit `-1/-1` marker plus a `review_notes` census naming each cause). It re-runs the daemon's own `classifyGitCommits` rather than a second implementation, and differences two canonical measurements (since tick start, since tick end) so a historical row reflects its own tick window instead of every commit the workdir landed afterwards

## [1.1.0] — 2026-09-03

> Tag `v1.1.0` → commit `ca9919574bf235cdbf2f32eeedfdb4c580b3c9e6` (annotated).
> Range `v1.0.0..v1.1.0` — **1117 commits** (~7.5 weeks of fleet operations).
> Composition: 427 board rows, 349 chore/bookkeeping, 106 fix, 57 foreman ticks,
> 52 feat, 39 docs, 16 gitreins, 10 test, 5 perf, 4 ci, 3 spec, 3 dogfood,
> 2 deps, 2 refactor, 2 security, 2 style. **0 breaking-change commits.**
> 169 distinct fleet row ids appear in the range.
>
> Summarised by category with row ids named — see the scope note above.
> The `[Unreleased]` section above (`167cc99d`, API conformance + verification
> harness) also landed in this release.

### Added — scheduling engine

- **Adaptive cooldown** — auto slow-down/speed-up by board progress (`c23963f9`); floor/ceiling/threshold as fleet.toml policy
- **Session resume after restart** — orphan detection + continuation nudges; `X-Hermes-Session-Key: <tick id>` on every spawn for durable tick↔session linkage (`98e8522c`, `6e31e7b3`, SCHED-GAP-074/091)
- **Idle-tick model routing** via `idle_model`/`idle_provider` (`0690b097`, SCHED-GAP-065)
- **Per-project daily/weekly/final budget enforcement** (`fe47d93d`, SCHED-GAP-066)
- **Board-aware pending-task urgency boost** in EVAL selection (`a47075f9`, SCHED-GAP-019)
- **Fleet.toml model/provider fallback chains** (`3e2c487e`, SCHED-GAP-064); ordered model chain (`1f53f44d`, SCHED-GAP-075) with a final-fallback rule when the chain is exhausted or empty (`80ad4c9e`, SCHED-GAP-076)
- **Task-router spawn resolution** — `router_spawn.py` head model/provider with fail-open chain fallback (`9f3cd4aa`, TASK-ROUTER-001)
- **Circuit breaker + retry/backoff** in the spawn failure path — 401/403 records a failure and advances to the next chain hop (`557b8292`, TASK-ROUTER-002)
- **Configurable foreman prompts** — namespace `default_prompt` + per-project append/replace (`374ffe51`)
- **Blackout windows** with `weekdays_only` for DeepSeek off-peak weekends (`92178bc1`); earlier blackout/slowdown hours for peak-pricing control (`49d4478e`)
- **Lazy skill loading** — tick prompt loads `coding-hermes-map` + `foreman` only (`d3cc6df5`)

### Added — namespaces, API, and control plane

- **Namespace-level `model_chain`** — a workspace tier between project and router (`5ad059c3`)
- **Per-namespace `max_concurrent` cap** — serializes duckbrain-sync ticks (`2d3b622a`)
- **Project groups + task templates** — JSONL blocks store wired into the daemon via `--groups-file`/`--templates-file`, with `/api/v1/groups|templates` and an idempotent deploy API (`80184ac7`, `8c5c5c4c`)
- **DELETE `/api/v1/namespaces/{id}`** with the full guard pattern — confirm gate, enabled-member 409, purge hard delete (`053ea50f`, `66f4cd96`, SCHED-GAP-097)
- **DELETE `/api/v1/projects/{name}`** soft delete with confirm + enabled guard, plus purge and failure-rate ghost filtering (`097eab2b`, `b0f34dc2`, DOGFOOD-005/009)
- **Disable provenance** — projects gain `disabled_at`/`disabled_by`/`disabled_reason` (migration v12), stamped on every disable path; resume clears them (`67fdccc3`, GAP-044), with a migration v13 backfill for pre-existing disabled rows (`af485947`, DOGFOOD-010)
- **Failure-rate auto-disable escalator** — `--failure-window`/`--auto-disable-*` flags with TOML and env equivalents, per-project breakdown in `/api/v1/status` (`4da16199`, `261410b2`, SCHED-GAP-018); armed-state visibility in status (`c613d3e4`, GAP-047)
- **Eval-stall watchdog** — forced re-evaluation + HIGH event when `lastEval` is frozen (`fb65daf6`, GAP-042), with one-cycle recoveries demoted to WARN (`57c7e2a3`, `d196cef9`)
- **Zero-select monitoring** — distinct `EVAL-ZERO-SELECT` log + HIGH event when 2 consecutive evals pick 0 while eligible projects exist (`9164a6e1`, GAP-043)
- **GET `/api/v1/config`** resolved-config introspection (`edf845f7`, SCHED-GAP-034)
- **Complete OpenAPI machine spec** — 19 paths at the time, per-operation request bodies, plus the 2 namespace sub-routes that had been missing (`7baa87a8`, GAP-057)

### Added — real tick telemetry

- **Real tick outcome metrics** (tokens, cost, commits) populated from gateway usage and git (`f965b617`, SCHED-GAP-029)
- **Tick cost from the router's PUBLIC per-hop price**, not a hardcoded map (`d2363a65`, SCHED-GAP-078)

### Added — DuckBrain sync

- **Interval flag + 429 backpressure** (`6e256f0a`); 429 treated as retryable with spool-and-replay, health tracking, and alerts (`e30e1b3b`, `b45b5267`)
- **Change detection** — skip unchanged writes per cycle (`790ad2f5`)
- **`X-API-Key` header support** via `DUCKBRAIN_API_KEY`, backwards-compatible when unset (`2a505529`, DB-GAP-039)
- **Startup key validation** — fail fast with a HIGH event on 401 (`8b5f076a`, SCHED-GAP-072)

### Changed

- **`go.mod` module path aligned** to `github.com/coding-hermes/scheduler` (`44a6d552`, GAP-059)
- **Cooldown-authority model reconciled** across `integration.md`, `system-plan-v2.md`, and the config docs — `fleet.toml` re-pins existing projects at every startup, and `fleet-cooldown-policy.py` is the only writer (`c840b4ae`, SCHED-GAP-025)
- **`--schema`/`--show-config` honesty** — effective env values shown, root TOML marked not-loaded until FEAT-005 (`024db0ea`)
- **Operator-set cooldowns are sacred** — `autoSlowdown` no longer escalates pinned tiers (`7cfddca4`)
- **Default foreman model** → `deepseek-v4-flash` on its GA release (`de63bda0`, `e814db63`); foreman provider now passed on gateway spawns so fleet ticks stop riding the main key (`f3919a70`)
- **Exec fallback disabled by default** (`--no-exec-fallback`) — reconnection re-engages HTTP after a fallback start (`b99e0ec3`, GAP-048)
- **Dependency bumps** — `modernc.org/sqlite` v1.54.0→v1.57.0 dropping the retracted libc v1.74.3 (`36ae6bd8`, DEP-001); 16 Go packages updated (`2ed4193e`)

### Fixed

- **Gateway-spawned ticks leaked as zombies** — placeholder session ID + heartbeat + 15-minute stale reaper (`ccea3f97`, S-GAP-003); startup cleanup now only reaps dead-PID ticks so live gateway ticks survive (`043a2b60`, INFRA-012)
- **Starvation boost made monotonic in starvation age** — most-starved p5 projects win slots over p10 ties (`2ff82c6a`, S-GAP-001)
- **Selection starvation + spawn-failure backoff** — every enabled project gets a tick attempt within its starvation window; consecutive failures back off exponentially to a 7200s cap (`9823acb3`)
- **Duplicate concurrent ticks** — running projects excluded from namespace packing (`66fef097`, INFRA-003); project-aware slot release so out-of-order completions stop evicting still-running markers (`1625c3d7`, SCHED-GAP-021)
- **Fallback packer re-spawned in-flight projects after restart** (`3fe90cb9`, SCHED-GAP-030)
- **Cooldown bypass for projects with only failed ticks** — `last_tick_completed` now updates for ALL terminal outcomes (`f05eddee`, `b64c3943`, CRITICAL-EDUOS-COOLDOWN)
- **Permanent exec-fallback loop** closed (`b99e0ec3`, GAP-048); custom-command spawns counted as exec spawns (they were undercounted since INFRA-004) (`79d76638`, GAP-049)
- **Bounded retry for transient gateway spawn failures** (`0ae94ed1`, SCHED-GAP-080)
- **Tick completion gated on session persistence + non-zero output** (`4c6b3a5c`, SCHED-GAP-079)
- **Early stdout pipe-close treated as clean end-of-output** (`32df989c`, SCHED-GAP-081)
- **Board closure-evidence gate** in the tick completion path (`ee78b451`, SCHED-GAP-085); dry-run board writer now commits its row (`563f8600`, SCHED-GAP-084)
- **Dry-run tick writer appended uncommitted rows** to a tracked file (`36c1e531`)
- **Dropped ticks emitted a HIGH spawn event** with an alert at ≥2 consecutive drops (`c6b36047`, GAP-050)
- **Shutdown drain** waits for / aborts in-flight gateway ticks and marks stuck rows failed (`db59b6a0`, SCHED-GAP-077)
- **PAYG foreman key defaulted at EVERY layer**, plus auth-vs-pair 401 discrimination (`a89162b6`)
- **Gateway spawns passed the foreman provider** instead of the main DeepSeek key (`f3919a70`)
- **`last_tick_started` stamped at spawn** in the gateway path — it had been written at session completion (`47afbb4d`, SCHED-GAP-060); exposed in `/api/v1/projects` so `standin-pick.py` stops reading "never" (`6baf946a`, SCHED-GAP-006)
- **Ghost projects prevented** — case-insensitive duplicate project names rejected at registration (`b0200d61`, SCHED-GAP-005); duplicate workdirs blocked at the API level with a decay-zero starvation guard (`863fafff`)
- **`fleet.toml` pins now actually pin existing projects** (`fd532c9d`); unassigned/dangling-namespace projects no longer silently dropped (`075fa993`)
- **Evaluation-cycle fixes** — real urgency in `GET /api/v1/queue` (`ade245fb`, GAP-054); hex colors from `urgencyColor`/`utilColor` (`6eac7f90`, GAP-055); packer cooldown predicate mirrored in zero-select eligibility (`c4b7dd1d`, GAP-050)
- **Migration `--help` and skip diagnostics** — eligibility filters documented and silent skips made visible (`0b9024c7`, `e16befc6`, DOGFOOD-017)
- **Simulation mode actually simulates** — `evaluate()` routes through the sim spawner with unique sim tick IDs (`85884ac1`); `--test-verify` no longer opens the production DB (`0a1a13f9`, DOGFOOD-002); `--test-verify` lazy slot-pool init for test environments (`d161f063`); state mutation guarded against unset env
- **MCP/API conformance** — `fleet_ticks` snake_case wire conformance (`f64ae7f5`, DOGFOOD-016); `fleet_add` defaults `Priority` and validates weight (`593bbf5d`, DOGFOOD-014); `fleet_add` accepts a `repo_url` alias (`ce248519`, DOGFOOD-019); canonical UTC tick id from the spawn endpoint (`7e99fb09`, DOGFOOD-015)
- **Dashboard** — namespace route returns 404 for an unknown namespace instead of 500 (`b563d863`, E2E-001); restart closes the liveness crash (`dbe22840`); SQL `MAX` misuse + int→bool scan bug (`38f4a09b`); a checkout move or typo'd symlink can no longer silently kill every `/fleet` slash command (`b0ad8d01`, DOGFOOD-008)
- **Schema default `cooldown_s` 7200 aligned with the loader** (`0ed3f5ac`, SCHED-GAP-033)
- **Integration test reads snake_case wire fields** (`03ec8ec5`, DOGFOOD-003 follow-up)
- **WAL checkpoints bounded** with a performance audit added to `--test-verify` (`176ad8d7`, DOGFOOD-006)

### Performance

- **Partial covering index** on `ticks(status, completed_at)` — `/api/v1/status` p99 under 100ms at production row counts (`6c34ae1b`, S-GAP-007)
- **Single-pass failure-rate aggregation** — kills a 44-query N+1 down to 1 (`5c40117c`, PERF-001), and the windowed CTE was reverted in favour of an indexed loop plus in-memory `last_evaluation` (`28f745e5`)
- **Dashboard per-project stats batched** (`93f7004c`, DASH-PERF-003); dashboard CI conclusions TTL-cached so the fleet overview renders under 3s (`c63779c2`, DASH-PERF-001); namespace panel N+1 fixed (`36255186`, AUDIT-014); queue tick lookup batched (`eb06ae15`); hot-path benchmarks added (`4577f648`, PERF-BENCH)

### Security

- **Live gateway API key and personal paths scrubbed from the systemd deploy unit** (`eebffe21`)
- **gitleaks allowlist narrowed** (`11dbdca5`)

### Documentation

- **`docs/api.md`** — standalone API reference covering every `/api/v1` route with params, bodies, and error semantics (`a430ec45`, GAP-056)
- **`docs/integration.md`** — operator integration guide (`fa6bbe4a`, SCHED-GAP-010/011/012)
- **`docs/scheduling-strategies.md`** — pluggable `SelectStrategy` design: current / EDF / cost-aware / lottery (`395c64c6`); plus the scheduling doctrine that cost never drives work selection (`ff41b08b`)
- **`docs/fleet-cost-governance.md`** — fleet cost governance spec (`7158e113`, SCHED-GAP-064..068)
- **`docs/skill-size-policy.md`** + fat-skill audit (`fdc1f0e9`, SCHED-GAP-067)
- **`docs/fleet.md`** regenerated repeatedly from the live API (`dc115447`, `6b98be8c`, `525a1c79`, `f2eb1ebb`, and others)
- **README** — stale operator numbers corrected (Go 1.26+, event-driven eval, 16 REST + 14 MCP endpoints) (`378a6511`, GAP-046); a fictional MCP tool table removed (`4946e775`, DOGFOOD-011)
- **`AGENTS.md`** — architecture guide added (`33044d52`), and the board-task selection model (`ecc2f9fc`, GAP-036)
- **Deployment docs** — gateway key out of argv, the local double-nesting layout note, and the `deploy/gateway.env.example` template (`6c3156bc`, GAP-038/039/040)
- **Queue eligibility semantics clarified** — the queue lists ALL enabled projects and `cooldown_s` is seconds-to-dispatchable (`98824379`, SCHED-GAP-063)

### Tests & tooling

- **Coverage build-out** — namespace CRUD (`220e95ac`, AUDIT-009), loop setters + packer helpers (`49a3f6d7`, AUDIT-010), gateway client (`efa71f86`, AUDIT-006), autoSlowdown (`322f9a99`, AUDIT-007), deliver + trimToolNoise (`1a08f80c`), DuckBrain sync (`0ea3e341`), `cmd/` packages (`e921c517`, AUDIT-016), regression tests for worker model defaults (`545effd4`)
- **Spawn-level gateway key regression guard** (`81748d85`, GAP-001)
- **Packer blackout integration tests** (`23acd120`)
- **CI hardened** against the `SA5011` `t.Fatal`-only nil-guard class across 7 files (`481709b7`, `0b30b977`, `990e0fd1`, `7176fa5f`)
- **Go toolchain pinned to go1.26.5** for 17 stdlib vulns (`2591fd40`, S-GAP-002)

## [1.1.1] — 2026-09-03

> Tag `v1.1.1` → commit `3ef83638493df01308165f1d12b439d0c5d4f3b2` (annotated).
> Range `v1.1.0..v1.1.1` — **1 commit**. Patch release.

### Fixed

- **Single-source build version — kill the stale `"1.0.0"` hardcode** (`3ef83638`). The v1.1.0 release shipped a broken version chain: the release workflow injected `-X main.Version` while `main` declared no such variable (a silent linker no-op), so every served identity was hardcoded — `/api/v1/health` and the MCP `serverInfo.version` both reported the stale literal `1.0.0` from a v1.1.0 binary, and no `--version` flag existed at all. Fixed by introducing the `internal/version` package (ldflags value wins; bare builds fall back to Go's native vcs buildinfo as `dev-<shorthash>`; `dev` last), adding `--version` to both `schedulerd` and `migrate`, pointing health/OpenAPI/`serverInfo` at the single `version.Current()` source, and correcting the `-X` import paths in the Makefile and `release.yaml` — plus the release-time version smoke that now fails the workflow when a binary does not report its tag.

## [1.2.0] — 2026-09-13

> Tag `v1.2.0` → commit `b6f7d4ef19251959fc6d031d7232a5e1aefe5712` (annotated).
> Range `v1.1.1..v1.2.0` — **167 commits**: 41 board rows, 74 foreman ticks,
> 13 feat, 10 fix, 7 chore, 5 docs, 5 test, 4 gitreins, 2 spec, 1 dogfood.
> **0 breaking-change commits.**

### Added — concurrent wave scheduling (SCHED-GAP-108..115)

- **Wave manifest ingest** (`a4973b28`, SCHED-GAP-110) — bounded, tolerant, attribution-only
- **Effective tick deadline** (`eea45194`, SCHED-GAP-111) — per-namespace `wave_tick_timeout` with a 4h ceiling and env override
- **Namespace `wave_workers_cap`** (`819e0476`, SCHED-GAP-113) — `WAVE_BUDGET` injection plus tick-boundary shed
- **Wave reaper** (`e5d227aa`, SCHED-GAP-114) — abandonment detection, capped recovery, recover-before-dispatch preamble
- **Wave cost attribution** (`e1ecdd25`, SCHED-GAP-115) — per-worker cost tracking, recovery-safe
- **`/api/v1/status` wave surface** (`a2c5fbc2`, SCHED-GAP-112) — `waves[]`, `wave_depth_total`, `tick_workers[]`
- **Migration v27 + wave model plumbing** (`0d3fc32a`, SCHED-GAP-109) — `worker_count`, wave config, `tick_workers`
- **Design doc** — `specs/S12-concurrent-wave-scheduling.md` (`29186dbf`, cost paths cited in §5.1 by `95fa5062`)

### Added — adaptive cooldown correctness

- **Adaptive cooldown measures commit anatomy, not commit counts** (`37c3a44f`, SCHED-GAP-104)
- **Board progress = net open-row decrease**, not row growth (`db7efd17`, SCHED-GAP-105)
- **Perpetual fixtures invisible to adaptive speed control** (`e861ec38`, SCHED-GAP-106)
- **Task bump** — temporary speed-up with auto-revert and API surface (`19b8ed7b`, SCHED-GAP-107), with `ApplyFleetConfig` skipping the cooldown re-pin while a bump is active (`fd430ef5`, SCHED-GAP-107)
- **Verification report** — zero-output tick count drops 89.7% (`e185e516`, revised metric `01897816`, SCHED-GAP-100)

### Added — zombie session reaper

- **Zombie session reaper for the coding-hermes scheduler** (`a9dad738`) — orphaned sessions are reaped instead of holding slots

### Added — projects API

- **`deliver` and `model_chain` exposed on `PUT /api/v1/projects/{name}`** (`d235e42a`, SCHED-GAP-095)

### Changed

- **Dead-knob sweep** — 4 unused config fields removed (`b2b5e884`, ADV-R02)
- **PM stand-in live scripts versioned** with a ledger-reconciliation helper (`c4faa352`, `3c5307cd`, GAP-048)

### Fixed

- **Zero-token gateway responses classified as failed** (`db4a01db`, SCHED-GAP-102) — previously recorded as completed
- **Same-project double-spawn TOCTOU** closed by reserving the project name at spawn (`e16f9718`, SCHED-GAP-103)
- **Redundant `/api/v1/resume` no longer wedges the eval loop** (`b4e52d09`, GAP-101)
- **Sim-setup actually exercises adaptive cooldown** and survives long runs (`00c2d69d`); dry-runs now exercise adaptive cooldown with dummy boards (`bf1b1b07`)
- **OpenAPI `Project` + `ProjectUpdates` schemas aligned** with the Go source of truth (`17682053`, SCHED-GAP-096)
- **CI-005** — flaky v27 migration wall-clock bound raised to a 2s smoke guard (`914ed95a`)

### Tests

- **INT-001** — adaptive cooldown isolation, config parse, and API roundtrip tests (`3ae010b5`)
- **INT-002** — handler-level integration tests for the blocks API: groups/templates/deploy CRUD, `dry_run`, torn JSONL, `-race` (`36e1b780`)
- **Adaptive cooldown full-cycle verification** (`955baf16`, Bane dry-run request), plus a bump verification suite with RED-check proofs (`38aa7dcc`)
- **SCHED-GAP-103 test wait condition** fixed — `RunningSet` now includes reserved names (`2204a65c`)

### Documentation

- **README** — `BROKEN/STALE` warning + release-check template (`265f961b`, GAP-042)
- **`AGENTS.md`** — the `--version` flag table row (`3d80ed71`, DOC-VERSION-1)
- **`docs/fleet.md`** regenerated from the live API (`90b1cdf3`)

### Ops

- **`fleet.toml`** — adaptive-cooldown keys added to the repo copy (`388b9131`, SCHED-GAP-100)
- **PM ledger drift split from open work** (`1efacf74`, MYPROJECT-GAP-049)
- **Dogfood** — `PROMISING-BUT-ROUGH` verdict; GAP-101 re-proven at HEAD and a sim-setup boot race filed as DOGFOOD-020..023 (`e914566f`)

## [1.3.0] — 2026-09-16

> Tag `v1.3.0` → commit `2c806dac2508d86d6c81ec19bbe72d942d52ec76` (annotated).
> Range `v1.2.0..v1.3.0` — **36 commits**: 20 board rows, 5 fix, 4 feat, 4 chore,
> 1 refactor, 1 test, 1 docs. **0 breaking-change commits.**
> **Note:** the tag push produced no release-workflow run (see `docs/releases.md` §5);
> the published artifacts were verified by re-running the workflow's version smoke
> against the downloaded binaries, and both report `v1.3.0`.

### Added — scheduler advancement (ADV-R03..R08)

- **Clock seam at the evaluate decision point** (`16514e13`, ADV-R04, G6) — deterministic time injection for evaluation, replacing scattered `time.Now()` reads
- **Git-verified board freshness reader** (`9d05dcfa`, ADV-R06, G11) — board state validated against git before trust decisions
- **Board-driven wake + ordering boost** (`7d6aa0b7`, ADV-R07, Option C) — newly filed board work wakes the scheduler early and boosts ordering
- **Observable slot-drop event + configurable slot patience** (`114161e8`, ADV-R08, G3) — dropped scheduling slots emit an event instead of vanishing silently

### Changed

- **Eligibility predicate single-sourced** (`bfb63ca1`, ADV-R03, G5), with a test pinning the `cooldown_s=0` semantics and documenting the intentional alignment (`32b6b28b`)

### Fixed

- **Per-turn deadline distinguishes active long turns from idle hangs** (`f09877d6`, SCHED-GAP-119)
- **Shorter per-turn deadline on the gateway POST** (`16c166d8`, SCHED-GAP-117) — gateway-unreachable ticks fail fast instead of burning the full tick timeout
- **Single canonical fixture registry read by `CountPending`** (`d58b49bc`, ADV-R05, G10)
- **pm-standin ops** — DuckBrain key passed via `DAGGER_ENV` with archive-then-truncate scratch accumulators (`36c257bb`), and `DAGGER_TOOL_MODEL` exported so the launcher honours the DAGGER-130 fail-closed route (`651560d2`)

### Security

- **SEC-001 closed** — leaked-credential scrub verified (tier2 PASS); scan noise 48→34, all documented false positives; rotation tracked as SEC-002 (`3292e244`)

### Documentation

- **`AGENTS.md`** — durable cooldown pins must exist in both stores: the DB ruling plus the `fleet.toml` pin (`dd977e67`, SCHED-GAP-121)

## [1.0.0] — 2026-07-18

### Core Scheduler

- **Dynamic priority-weighted fleet scheduler** — single Go binary replaces 33+ static cron jobs
- **Urgency-based packing** — greedy knapsack fill with configurable weight budget and max concurrency
- **Geometric priority curve** — priority 1-10 maps to intervals from 20 minutes to 24 hours
- **Dynamic cooldown** — derived from priority when explicit cooldown is 0, preventing starvation
- **Auto-slowdown for idle projects** — doubles cooldown after consecutive idle ticks (capped at 4h), resets on first non-idle tick
- **Process-liveness zombie detection** — `/proc/pid/stat` instead of blind timeouts (30min cap rejected)

### HTTP API Spawn (FEAT-003)

- **Zero subprocess overhead** — `POST /v1/responses` to Hermes gateway instead of `exec.Command`
- **No MCP duplication** — duckbrain + gitreins loaded once by gateway, shared across ticks
- **~500MB → 0MB per tick** process overhead eliminated
- **Graceful fallback** — exec.Command when gateway is unreachable
- **Per-foreman MCP optimization** — HERMES_HOME with minimal config (duckbrain+gitreins only, no browser/chimera/flights)

### Dedicated Gateway (FEAT-004)

- **Cgroup isolation** — separate Hermes instance on :8643 with MemoryMax=16G
- **Independent restart cycle** — scheduler OOM doesn't kill main chat
- **Scheduler profile** — minimal MCPs, auto-approve mode, PAYG foreman provider

### API & Control Plane

- **REST API** — 15 endpoints (health, projects CRUD, ticks, events, evaluate)
- **MCP server** — 14 fleet_* tools at `/mcp` endpoint (status, projects, weight, priority, pause/resume, ticks, evaluate)
- **Dark theme dashboard** — HTML at `/` showing fleet status, project cards, tick history
- **Hermes plugin** — `/fleet` slash commands (status, weight, priority, pause, resume, ticks, evaluate)

### Configuration & Infrastructure

- **TOML fleet config** — `--config fleet.toml` for declarative project/namespace seeding
- **Cron migration tool** — `cmd/migrate/` imports Hermes cron jobs.json into SQLite
- **Multi-namespace DuckBrain** — separate namespaces per project with read-replica sync
- **Systemd deployment** — user units with MemoryMax, Restart=always, journal logging
- **Built-in verification** — `--test-verify N` with temp DB, 7-project fleet, 6 invariant checks

### Quality & Reliability

- **Goroutine leak fix** — context-cancellable stdout scanner, explicit pipe closure, tick timeout
- **Memory optimization** — per-chat MCP reduction (500MB → 175MB), MemoryMax=32G for 8 concurrent
- **pprof debugging** — net/http/pprof endpoint for production diagnostics
- **Alert escalation** — configurable thresholds with event emission
- **SQLite schema migrations** — versioned with automatic upgrade path
- **Built-in simulation** — `SimSpawner` for testing without real subprocesses

### Developer Experience

- **Makefile** — build, test, test-full, lint, fmt, migrate, deploy
- **Go 1.26** — latest stable toolchain
- **Vulnerability scanning** — govulncheck integration
- **Conventional commits** — feat/fix/docs/chore with co-author template
