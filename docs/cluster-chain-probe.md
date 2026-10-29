# Cluster chain probe — measuring the distributed code factory's hops

`ops/cluster-chain-probe.sh` (and its companion `ops/cluster-chain-probe-selftest.sh`)
is the executable measurement for the distributed-code-factory goal
(`~/.hermes/goals/remote-coding-system.md`). That goal names six done-gates
(G1–G6) for a chain in which a task is routed to a model, dispatched by the
scheduler, carried over the crier bus, executed inside a bunker agent and
landed through a connector. Before this probe, every claim about the chain was
prose: nothing could be re-run, nothing could disagree with the operator, and
nothing could go red.

The probe measures **one hop per arm**. It answers "does this hop answer, and
what did it answer with" — not "did a task travel end to end". That distinction
is the whole reason the non-coverage list below exists.

## Contract

```
ops/cluster-chain-probe.sh <arm> [options]
  arm ∈ router | proxy | crier | scheduler | bunker | connector | all | arms
```

One JSON object per arm on its own line, shape:

```json
{"arm":"crier","verdict":"PASS","measured":"url=http://127.0.0.1:8767 health=ok version=0.1.0-rc2-417-gac00769 commit=ac007694 auth=refused:401","reason":"bus reachable and build-attributable; no credential sent","elapsed_ms":52,"ts":"2026-10-28T06:55:00Z"}
```

* `verdict` — `PASS`, `FAIL`, or `REPORT`.
* `measured` — **what was actually read**. A PASS with an empty or vague
  `measured` is a probe bug, not a green: the selftest asserts the field
  carries the observed value (the stub's version string), never a bare `ok`.
* `reason` — empty on an ordinary PASS; on a FAIL, a stable
  `CODE: human sentence` prefix so a wrapper can match on the code.
* `elapsed_ms` — wall time for that arm, bounded by `--timeout` per external
  command.

`all` additionally prints a summary object and one human line, then exits:

```json
{"arm":"summary","verdict":"FAIL","passed":3,"failed":1,"report_only":1,"gating":["router","proxy","crier","scheduler","bunker"],"arms":{...},"overall_exit":1,"ts":"..."}
CLUSTER-CHAIN-PROBE: FAIL — router=PASS proxy=PASS crier=PASS scheduler=FAIL[SCHEDULER_BUILD_DRIFT] bunker=PASS connector=REPORT(non-gating)
```

Exit codes: `0` every gating arm that ran PASSed · `1` at least one gating arm
FAILed · `2` usage error. The `connector` arm is **report-only** and never
gates — see its row below for why. The `proxy` arm gates on drift it can measure
and reports when it cannot (no listener, no loaded commit, no checkout); pass
`--proxy-required` on a host that owns the ingress to make those cases FAILs.

### Read-only by construction

Every arm issues a GET, or an explicitly read-only CLI verb (`bunker list`).
Nothing in the probe restarts, deploys, writes, signals or mutates a service, an
agent or a board, so it is safe to run against the live fleet at any time. It
never reads a credential and never sends one: where a surface requires auth
(crier `/status`, `/agents`) the probe deliberately sends **no** `Authorization`
header and records the refusal as the measurement. The selftest proves both
halves — the stub records every request and the test asserts the probe only ever
sent `GET`, and a fake token in the environment never appears in the output.

## Arms: what each one measures, and what it does not

### `router` — model selection

**Measures, two surfaces:**

1. The CLI hop: `~/.hermes/scripts/router_spawn.py <project> --format json`
   (falls back to `~/task-router/scripts/router_spawn.py`). PASS requires the
   answer to name **both** `head.model` and `head.provider`.
2. The documented fleet API (`task-router/docs/health-plane.md`: listener
   `127.0.0.1:9092`): `GET /resolve?project=<project>` must answer `200` with a
   model+provider pair, and `GET /resolve?project=<unknown>` must refuse with
   `400`. The probe reports `surfaces_agree=yes|no` rather than hiding a
   disagreement between the two.
3. Refusal with a **named code**: `--profile-req cluster_probe_unknown_category=3`
   must return a `code` field (measured live: `INVALID_REQUIREMENT`). A refusal
   that cannot be named cannot be told apart from a crash.

**PASS means:** the router resolved a route for the named project/profile, named
the model and the provider, refused an invalid request with a code, and the HTTP
surface agreed.

**FAIL codes:** `ROUTER_SCRIPT_ABSENT`, `ROUTER_PYTHON_ABSENT`,
`ROUTER_CLI_EXIT_<n>`, `ROUTER_REFUSED_HAPPY_PATH`, `ROUTE_UNNAMED`,
`ROUTER_REFUSAL_UNNAMED`, `ROUTER_HTTP_UNMEASURED`, `ROUTER_HTTP_UNREACHABLE`,
`ROUTER_HTTP_UNNAMED`, `ROUTER_HTTP_REFUSAL_UNNAMED`.

**Does NOT cover:** that the chosen model is *good*, that it has quota left, or
that a spawn with that model succeeds. It reads the registry's answer; the
`:9391` classified proxy is out of scope. `--no-router-http` skips surface 2
(a CLI-only host), which the `measured` field states explicitly.

### `proxy` — the classified model-serving ingress

**Measures:** `GET <proxy>/health` on the classified proxy (`task-router-proxy`,
`127.0.0.1:9391`, env `ROUTER_PROXY_URL`) must answer `200`; its `status` must be
`ok`; the loaded commit is read from `commit` (falling back to
`code.loaded_commit`). The arm then compares that commit against the ingress's
own **deploy lineage** — `origin/main` of `~/task-router` (or `--proxy-repo`) —
over the paths that can reach a running interpreter:

* ref starts with the loaded commit → `drift=0`, PASS.
* loaded commit resolves here → PASS, or FAIL `PROXY_BUILD_DRIFT` **naming both
  commits**, the raw `behind`/`ahead` counts, and `compiled_behind` /
  `compiled_ahead` counted over `*.py`.
* loaded commit is not a commit in that checkout → FAIL
  `PROXY_BUILD_UNKNOWN_COMMIT`.
* `200` with `status != ok` → FAIL `PROXY_UNHEALTHY`, whatever the build is.

**Why it exists:** the ingress is the one hop whose staleness is invisible by
construction. `/health` has carried an honest `code` block since TR-141 and a
watchdog has consumed `code.stale` since TR-255, yet the proxy served a build
loaded from an older commit with no probe arm able to say so — measured
2026-10-29 19:35Z: `loaded_commit=13ef7482e1d7`, `repo_commit=73d4795b0dc9`,
`stale=true`, while every other arm the chain probe looked at was green. The
`measured` field records `stale_flag` and `flag_agrees=yes|no`, so a flag that
disagrees with the measurement is printed rather than believed.

**Only `*.py` counts.** A Python process imported the files that were on disk
when it started, so a commit that touches no Python source cannot be in it by
definition. Counting board or docs commits after a deploy as drift is exactly
the noise that made the stale ingress invisible; both raw counts are still
printed, so nothing is hidden.

**PASS means:** the ingress is answering, healthy, and serving a build that
matches the deploy lineage for every commit that reaches a running Python
process.

**FAIL codes:** `PROXY_UNREACHABLE`, `PROXY_UNHEALTHY`, `PROXY_NO_COMMIT`,
`PROXY_DRIFT_UNMEASURED`, `PROXY_BUILD_DRIFT`, `PROXY_BUILD_UNKNOWN_COMMIT`.
`PROXY_*_UNMEASURED`/`PROXY_UNREACHABLE`/`PROXY_NO_COMMIT` are only a FAIL when
`--proxy-required` (`CLUSTER_CHAIN_PROXY_REQUIRED=1`) is set; otherwise the
verdict is `REPORT` and the arm prints `REPORT_ONLY: … exit code unaffected`. A
host that does not run the ingress must not read a missing hop as a broken one;
a host that owns it must not accept a report where a verdict is possible.

**Does NOT cover:** that a completion was served, that any model answered, that
the deployed ingress is the *newest* build (only that it is not behind the
lineage on compiled paths), or that a restart would succeed. Like every arm it
measures and never deploys — a `PROXY_BUILD_DRIFT` is fixed with sync + restart,
by an operator.

### `crier` — the bus

**Measures:** `GET /health` must answer `200` with `status: ok`; `GET /version`
must answer `200` with a version string (so the listener is attributable to a
build, not just "something is on the port"); `GET /status` unauthenticated
records the auth posture — `auth=refused:401 …` when auth is enforced,
`auth=disabled:…` when the route answers unauthenticated, `auth=inconclusive:…`
for any other status.

**PASS means:** the bus is reachable, healthy, build-attributable, and the auth
posture is recorded.

**FAIL codes:** `CRIER_UNMEASURED`, `CRIER_UNREACHABLE`, `CRIER_UNHEALTHY`,
`CRIER_VERSION_UNAVAILABLE`, `CRIER_VERSION_UNNAMED`.

**Does NOT cover:** a message was published, delivered or acked. The probe never
authenticates, so it never reads inboxes, agent lists or relay topics. It also
does not verify mesh peers or federation.

### `scheduler` — dispatch

**Measures:** `GET /api/v1/live` first — the DB-free liveness route
(SCHED-GAP-204-A), which answers from process memory and carries `build_sha` and
uptime. Only that route's failure is `SCHEDULER_UNREACHABLE`. Then
`GET /api/v1/health` (the rich, DB-backed route): `200` gives `status: ok`,
`db: connected` and the full field set; a `503`/`504` is recorded as
`health=degraded:<code>(<helper>)` with `db=unknown` and the arm carries on —
`/api/v1/health` runs its DB reads under `--api-read-timeout` (default 5s) on
one serialized SQLite connection, so a busy fleet makes it answer
`{"error":"deadline exceeded","helper":"PingContext"}` while the daemon is
perfectly alive. `GET /api/v1/status` supplies `active_ticks`,
`active_projects` and its own `build_sha`.

Then it compares the **running** `build_sha` against the checkout's
**deploy lineage** — `origin/main` by default, or `--against REF`:

* the ref starts with `build_sha` → `drift=0`, PASS.
* `build_sha` resolves to a commit here → FAIL `SCHEDULER_BUILD_DRIFT` **naming
  the count and the ref**: measured live as `behind=112 ahead=0 against=origin/main`.
* `build_sha` is not a commit in this checkout → FAIL
  `SCHEDULER_BUILD_UNKNOWN_COMMIT`.

Comparing against the branch you happen to have checked out is the trap: from a
feature worktree a correctly deployed daemon reads as drift. The `measured`
field always carries `against=<ref>`.

**Only commits that reach the binary count.** A commit touching no Go source,
module file or embedded template cannot be inside the daemon by definition, so
the arm also reports `compiled_behind` / `compiled_ahead` over
`*.go *.mod *.sum *.html *.tmpl` and PASSes when those are 0 — otherwise an ops
script or a doc fix landed after the deploy reads as "the fleet runs a build the
lineage does not contain". Both counts are always printed, so the raw drift is
never hidden.

**PASS means:** the daemon is healthy, its database is connected, and the binary
answering is the binary this tree builds.

**FAIL codes:** `SCHEDULER_UNMEASURED`, `SCHEDULER_UNREACHABLE` (the DB-free
liveness route failed), `SCHEDULER_UNHEALTHY`, `SCHEDULER_DB_NOT_CONNECTED`,
`SCHEDULER_NO_BUILD_SHA`, `SCHEDULER_HEALTH_UNEXPECTED`,
`SCHEDULER_STATUS_UNAVAILABLE`, `SCHEDULER_DRIFT_UNMEASURED`,
`SCHEDULER_BUILD_DRIFT`, `SCHEDULER_BUILD_UNKNOWN_COMMIT`. A `504` on the rich
health route is NOT one of them: it is recorded as `degraded` and does not fail
the arm.

**Does NOT cover:** that any tick was dispatched, admitted or completed, that a
project is configured correctly, or that a spawn reached the gateway. Active
tick counts are reported as telemetry, not as proof of dispatch. When the probe
runs from a feature worktree, its HEAD is that worktree — a deploy built from
main will legitimately report drift, which is exactly what the arm is for.

### `bunker` — execution host

**Measures:** `bunker list` (read-only) resolves the CLI (`$BUNKER_BIN`, then
`PATH`, then `~/.local/bin/bunker`), exits 0, and prints a
`Total: N agents (server: …)` line. Reported as `server=<name> agents=<n>`.

**PASS means:** the agent host answered through its CLI and its agent census was
read.

**FAIL codes:** `BUNKER_CLI_ABSENT`, `BUNKER_CLI_EXIT_<n>`, `BUNKER_NO_TOTAL_LINE`.

**Does NOT cover:** that an agent is healthy, that a command can be executed
inside one, disk/quota headroom, or any per-agent state beyond the list line.
No agent is created, started, exec'd into or destroyed.

### `connector` — report-only

**Measures presence, twice:** does `internal/connector` exist on the checked-out
tree (directory + tracked file count), and does `origin` carry
`refs/heads/wt/SCHED-HARNESS-003-port` (`git ls-remote`, skipped by `--offline`).
Verdict is `REPORT`, never `PASS`/`FAIL`.

**Why it never gates:** a report-only arm that could pass would claim a hop
works when all it proved was that a directory and a branch exist. The connector
is the one hop with no runtime to probe, so the arm's honest output is a fact,
not a verdict. Its `reason` always says `REPORT_ONLY: … never gates the exit
code`.

**FAIL/other codes:** `CONNECTOR_UNMEASURED` (no checkout to inspect).

**Does NOT cover:** anything about a live connector — no run, no deployment, no
lane, no queue.

## Reason codes at a glance

Every FAIL reason begins with one of: `ROUTER_*`, `PROXY_*`, `CRIER_*`,
`SCHEDULER_*`, `BUNKER_*`, `CONNECTOR_*`, or `PROBE_INTERNAL` (an arm returned
without a verdict — a probe bug, never a silent pass). Wrappers should match the prefix
before the colon.

## What the probe does NOT cover, overall

* **End-to-end travel.** No tick is spawned, no task is written to a board, no
  message is published, no agent executes anything. A green chain means every
  hop *answers*, not that work *flowed*.
* **Authentication.** The probe never presents a credential, so it cannot see
  authenticated state: crier inboxes, agent registries, protected scheduler
  routes.
* **Correctness of a decision.** A route can be resolved, a daemon healthy and
  an agent listed while the model is unsuitable, the project misconfigured or
  the agent out of disk.
* **The connector hop**, beyond presence (see above).
* **Time.** One sample per arm; it does not trend, average or alert. Run it from
  cron and diff the JSON if you want history.

## Between samples: the hop liveness watchdog

A hop can exit **cleanly** and stay dead (`Restart=on-failure` never applies to
exit 0) with nothing noticing until someone runs this probe by hand — that is how
`crier-fleet` sat inactive for 11 hours (SCHED-GAP-16946).

`ops/hop-liveness-watch.sh` + `deploy/hop-liveness-watch.timer` watch the gating
units once a minute from systemd and append a named alert when one goes down:
`docs/hop-liveness-watchdog.md` is the authority.

The two instruments are complementary and neither is a substitute for the other:
the watchdog proves a unit is **running** every minute, the probe proves each hop
**answers** on demand. The watchdog's crier arm carries no HTTP check and the
probe's crier arm carries no timer.

## Running it

```sh
ops/cluster-chain-probe.sh all                     # whole chain, live
ops/cluster-chain-probe.sh scheduler               # one hop, exit code tells the story
ops/cluster-chain-probe.sh all --offline           # no git ls-remote
ops/cluster-chain-probe.sh all --repo ~/coding-hermes-scheduler/coding-herms-scheduler \
                                --timeout 20
ops/cluster-chain-probe.sh proxy --proxy-required      # the host owns :9391
CRIER_URL=http://127.0.0.1:8768 SCHEDULER_URL=http://10.0.0.5:9090 \
  ops/cluster-chain-probe.sh crier scheduler
```

Every external command is bounded by `--timeout` (default 15s) and no arm can
leave a process behind: the probe starts nothing.

## The selftest — how we know the probe can say no

```sh
ops/cluster-chain-probe-selftest.sh            # 117 checks, exit 0
ops/cluster-chain-probe-selftest.sh --verbose  # shows every probe output
```

It drives the real probe against targets it controls, on `127.0.0.1` only (a
python stub HTTP server, a black-hole socket that accepts and never answers, and
closed ports), so it needs no fleet and no external network:

1. **Causality.** The *same* invocation, pointed at the *same* URL, PASSes while
   the stub answers and exits 1 with `CRIER_UNREACHABLE` once the stub is killed
   and the port is proven released. Only the target's liveness changes.
2. **A PASS is a measurement.** The PASS object's `measured` field must carry
   `version=selftest-stub-1.2.3` — the value the stub served — so a PASS can
   never be a bare `ok`.
3. **Drift is counted, in both directions.** Against a purpose-built 3-commit
   repo and a stub serving its HEAD sha, the scheduler arm PASSes with
   `drift=0` and names the `build_sha` it read; serving `HEAD~2` instead makes it
   exit 1 with `SCHEDULER_BUILD_DRIFT` and `drift=behind=2 ahead=0`; an
   unresolvable sha gives `SCHEDULER_BUILD_UNKNOWN_COMMIT`.
4. **Every gating arm can fail by name.** `SCHEDULER_UNREACHABLE` on a closed
   socket, `ROUTER_SCRIPT_ABSENT` with no `router_spawn.py`, `BUNKER_CLI_ABSENT`
   with no CLI, and a dead-everything `all` run that reports `failed:4`,
   `overall_exit:1` and every one of those codes.
5. **Bounded.** A socket that accepts and never answers fails the arm in
   ~3s under `--timeout 3`, not on a hang.
6. **Read-only and quiet.** The stub records every request; the test asserts the
   probe sent GETs only, and that fake `CR_AUTH_TOKEN` / `OPENROUTER_API_KEY`
   values in the environment never appear in the output.
7. **The connector arm never gates.** Exit 0 and `verdict:REPORT` both with
   `internal/connector` present and absent.
8. **The proxy arm measures the ingress, in both directions.** Against a
   purpose-built 4-commit Python repo and a stub serving its tip, the arm PASSes
   with `drift=0`; serving `HEAD~2` makes it exit 1 with `PROXY_BUILD_DRIFT`,
   `compiled_behind=2` and **both** commits named; serving the commit from before
   a docs-only commit PASSes with `compiled_behind=0` while still printing
   `behind=1`. A stub whose `stale` flag lies (`false` over a behind build) still
   FAILs and records `flag_agrees=no`; an unattributable commit gives
   `PROXY_BUILD_UNKNOWN_COMMIT`; `200` with `status=degraded` gives
   `PROXY_UNHEALTHY`; a `200` naming no build gives `PROXY_NO_COMMIT` as a
   `REPORT`. Replacing the drift comparison with `p_cb="0"` in a throwaway copy
   turns the stale case green — so the checks are attributable to it.
9. **A complete fresh chain can say yes.** With a fresh stub, a route stub and a
   bunker stub, `all` PASSes five gating arms (`passed:5`), reports the
   connector (`report_only:1`) and exits 0. Green is a reachable state, not just
   a theoretical one — the dead-everything run in check 4 proves the same
   summary can say `failed:5`.

**The selftest itself is proven non-vacuous.** Neutering the probe's
`CRIER_UNREACHABLE` branch to `arm_pass` in a throwaway copy turns the selftest
red — measured on this revision: `SELFTEST: PASS=53 FAIL=7`, exit 1, with the
failing checks naming the causality, reason-code, timeout and summary
assertions. A selftest
that stayed green over a neutered probe would be the exact failure this
doctrine exists to prevent.

## Reading a live run

Measured on the fleet host 2026-10-29 19:39Z (`ops/cluster-chain-probe.sh all`):

| arm | verdict | measured |
|---|---|---|
| router | PASS | `model=openai/gpt-5.6-terra provider=xkiro profile=P1_CODING hop=1` (CLI and `:9092` agree; unknown project → 400) |
| proxy | **FAIL** | `loaded_commit=13ef7482e1d7 repo_commit_at_load=73d4795b0dc9 stale_flag=true head=73d4795b drift=behind=3 ahead=0 compiled_behind=2` → `PROXY_BUILD_DRIFT` |
| crier | PASS | `version=0.1.0-rc2-417-gac00769 commit=ac007694 auth=refused:401` |
| scheduler | **FAIL** | `build_sha=674d26ed … head=59a84d45 drift=behind=12 ahead=0` → `SCHEDULER_BUILD_DRIFT` |
| bunker | PASS | `server=bunker-mvp agents=4` |
| connector | REPORT | `internal/connector=absent tracked_files=0 origin=present:236a84ca` |

Re-measured on the fleet host 2026-10-29 19:39Z, adding the `proxy` arm: the
ingress had been serving a build two Python commits behind `origin/main` with
nothing on the chain saying so. Both FAILs in that table are real deploy gaps,
not probe defects — the probe measures and never deploys.

Both FAILs are real findings, not probe defects: the scheduler daemon was built
12 commits before the tree, and the ingress had loaded a build two `*.py` commits
before it. Fixing either is a deploy+restart, which this probe deliberately never
performs — see [runbook-drain-restart.md](runbook-drain-restart.md) and
`ops/check-daemon-freshness.sh` for the daemon path; the ingress is
`make restart-router` in `~/task-router`, verified by its own `/health`.
