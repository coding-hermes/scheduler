# Muster protocol integration — scope decision (SCHED-GAP-1579)

**Status:** DECIDED — 2026-10-16. This is the input the spec row (`SCHED-GAP-1580`) is built on.
It changes no code, modifies no served surface, and starts no implementation row.

**Design authority honored:** `muster/specs/PROTOCOLS.md` ("no protocol implementation row may
start before its adapter satisfies this spec's pipeline and cross-cutting contracts on paper").
The muster side of every claim below is grounded in that spec plus measured behavior of the real
`openapi-mcp` binary (v0.1.1-468-g2da20117) driven against the live scheduler daemon
(v1.12.0-410-g33ceee01, `127.0.0.1:9090`) during this scoping pass. Every acceptance command in §3
was executed live while writing this document; results are quoted where they decided a question.

**The scheduler side is grounded in:** the served spec (`GET /api/v1/openapi.json`, 41 paths /
54 operations, fetched live), the route table in `internal/api/server.go`, the mutation gate
(`internal/api/auth_gate.go`, SCHED-GAP-1602) and its credential model (`internal/api/auth.go`),
the MCP mutation classification (`internal/mcp/auth.go`, SCHED-GAP-1619), the spec parity tests
(`internal/api/server_test.go::TestAPI_OpenAPI_Success`), and the SCHED-GAP-1575-B read-deadline
wiring (`--api-read-timeout`, default 5s).

---

## 1 · SURFACE — what muster exposes, and why

**Decision: muster consumes the SERVED spec as-is — all 41 paths / 54 operations, no spec
trimming, no forked "consumer spec" maintained by hand.** The safety boundary is the daemon's
operator-credential gate (server-side, fail-closed, audited), not document surgery on the
consumer side.

Why no trimming:

1. **The gate is already server-side and build-pinned.** Every mutating operation is refused
   without the credential by `requireOperator` *inside the handler* (SCHED-GAP-1602) — the gate
   lives in the handler, not middleware, so it holds regardless of what any client believes the
   spec says. Re-implementing the mutating/open split as a spec filter in a derived consumer doc
   would create a SECOND classification that can drift from the first
   (`internal/api/auth.go` ROUTE SPLIT + `internal/mcp/auth.go::mutatingTools`, which a coverage
   test pins against the live tool registry). One classification, served through the spec's
   per-operation `security` markers (all 27 mutating operations carry
   `security: [{"operatorToken": []}]` in the served spec today), is the maintainable shape.
2. **Spec drift degrades documentation, never enforcement.** If a future mutating route lands
   without its `security` marker, muster consumers would see it as callable — and then receive a
   well-formed 401/503 refusal from the daemon. That is the correct failure direction. (Guard
   row for the spec: when a mutating route lands, its spec entry must carry `security` — same
   review discipline the parity tests already enforce for path presence.)
3. **The served document stays the single contract.** A hand-trimmed fork rots on the first route
   change and silently disagrees with the dashboard/docs about what exists.

**Explicitly NOT exposed through MCP (and why):**

- **The two SSE routes as MCP-invoked operations** — `GET /api/v1/events/stream` and
  `GET /api/v1/observatory/stream`. The generic OpenAPI pipeline registers them as tools (muster
  has no per-operation veto in the OpenAPI path), but they are **forbidden by policy** under
  muster's agent-safety contract (PROTOCOLS.md §4.5): rule 1 requires every MCP-exposed operation
  to return or be bounded, and rule 4 forbids `KindEventSource`-shaped operations as long-lived
  MCP calls. These two are the spec's event-source analogues. The mechanical bound: muster's
  executor calls them through an `http.Client` whose `Timeout` is `MUSTER_HTTP_TIMEOUT`
  (seconds; default 30) — a mistaken SSE tool call RETURNS as an error transcript within that
  budget rather than wedging the agent loop (test T5 in §3 pins this). Steady-state consumers use
  the bounded poll surrogates instead: `GET /api/v1/events?limit=N` and `GET /api/v1/observatory`
  — both plain request/response, both already tools.
- **Anything not in the OpenAPI spec** — the htmx dashboard, `/dashboard/*`, and static assets.
  Muster consumes OpenAPI only; the dashboard is a human surface.
- **Nothing else.** In particular, the destructive operations (project delete with
  `purge=true`, namespace delete, `groups/{name}/deploy`, fleet pause/resume, spawn, evaluate)
  are deliberately all PRESENT in the consumed surface: an operator-credentialed muster consumer
  is the operator, and an agent consumer without the credential cannot reach them (§4). Hiding
  them from the document would only hide the refusal an agent needs to see.

**Tool-shape consequence pinned NOW (avoids a breaking change later):** the served spec carries
**no `operationId` on any operation**, so muster names tools `method_path`
(`get_api_v1_live`, `post_api_v1_projects_name_pause`, `delete_api_v1_projects_name`, … — 54
measured). Muster prefers `operationId` for tool names when present; ADDING operationIds later
renames every tool and breaks recorded agent sessions (PROTOCOLS.md §5.3: MCP tool names stay
byte-identical). **Decision: do not add operationIds in v1.** If the spec row ever wants prettier
names, that is a deliberate, separately-gated breaking change.

---

## 2 · CONSUMER CONFIG — the exact block

Two states. The **target state** consumes the served spec over HTTP; it is blocked today by one
measured defect (SPEC-ROB-001 below). The **works-today state** uses a derived copy. Both blocks
are verbatim-valid JSON.

### 2.1 Target state (after SPEC-ROB-001 lands)

```json
{
  "mcpServers": {
    "coding-hermes-scheduler": {
      "command": "openapi-mcp",
      "args": [
        "--spec", "http://127.0.0.1:9090/api/v1/openapi.json",
        "--base-url", "http://127.0.0.1:9090"
      ]
    }
  }
}
```

Agent-facing consumers stop here — no credential in the block (§4). An operator-facing consumer
adds the credential as a flag pointing at a 0600 file (never inline the token in a config that
may be shared or synced):

```json
{
  "mcpServers": {
    "coding-hermes-scheduler-operator": {
      "command": "openapi-mcp",
      "args": [
        "--spec", "http://127.0.0.1:9090/api/v1/openapi.json",
        "--base-url", "http://127.0.0.1:9090",
        "--auth-token-file", "/etc/muster/scheduler-operator-token"
      ]
    }
  }
}
```

### 2.2 Works-today state (derived copy) — use this until SPEC-ROB-001 lands

```json
{
  "mcpServers": {
    "coding-hermes-scheduler": {
      "command": "openapi-mcp",
      "args": [
        "--spec", "/etc/muster/scheduler-openapi-consumer.json",
        "--base-url", "http://127.0.0.1:9090"
      ]
    }
  }
}
```

The derived copy is **generated, never hand-edited, never committed to the scheduler repo**:

```sh
jq 'del(."security-note")' \
  <(curl -sS http://127.0.0.1:9090/api/v1/openapi.json) \
  > /etc/muster/scheduler-openapi-consumer.json
```

Regenerate it whenever the daemon version changes (the doc's `info.version` carries the build
SHA, so staleness is detectable by diffing `info.version` against `GET /api/v1/live`'s).

**Why the derived copy exists at all — SPEC-ROB-001 (measured, reproducible):** the served spec
carries a non-standard top-level sibling field, `"security-note"` (the SCHED-GAP-1602 prose
block). Muster loads the spec through kin-openapi and **validates it strictly**
(`cmd/mcp-server/main.go::loadSpec` → `doc.Validate`); URL loading is fully supported
(`LoadFromURI`), but validation rejects the document before a single tool is generated:

```
$ openapi-mcp --spec http://127.0.0.1:9090/api/v1/openapi.json --base-url http://127.0.0.1:9090
Fatal error: failed to load spec: validate spec: extra sibling fields: [security-note]
```

With the sibling removed, the same binary over the same spec generates cleanly:

```
Loaded spec: Coding Hermes Scheduler API (v1.12.0-410-g33ceee01)
Generated 54 MCP tools
Using base URL: http://127.0.0.1:9090 (override=true)
```

**SPEC-ROB-001 (spec-row work, not this doc): move the security-note prose into a
spec-legal location** — per-operation `description` text (the operators already carry the
credential prose in `components.securitySchemes.operatorToken.description`) or an `x-` extension
(`x-security-note`), which kin-openapi accepts. One-line change to the embedded
`openapiSpec` constant in `internal/api/server_helpers.go` plus the parity-test update. This
document deliberately does NOT modify the spec.

**Config rules (pinned):**

- `--base-url` is **always explicit**, never omitted: it overrides the spec's `servers[]`, and
  pinning it defends against a stale spec pointing a consumer at the wrong instance.
- `--spec` by URL is the target shape (zero derived state); by file only until SPEC-ROB-001.
- Tool count is a contract: the startup line `Generated 54 MCP tools` is asserted in §3. A
  different number means the surface drifted and the acceptance re-run must explain the delta.

---

## 3 · ACCEPTANCE TEST — commands and non-vacuous assertions

Run on the scheduler host. `openapi-mcp` is `~/wojons/muster/bin/openapi-mcp` (built by
muster's `make build-mcp`). The wire protocol is newline-delimited JSON-RPC 2.0 over
stdin/stdout (`pkg/mcp/server.go`), which is exactly what an MCP client speaks, so a shell probe
is a faithful stand-in for a real client.

### T1 — the spec loads through muster's real validator (and the tool surface is the whole API)

```sh
curl -sS http://127.0.0.1:9090/api/v1/live          # preflight: daemon up
jq 'del(."security-note")' \
  <(curl -sS http://127.0.0.1:9090/api/v1/openapi.json) \
  > /tmp/scheduler-openapi-consumer.json

printf '%s\n%s\n%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
 | ~/wojons/muster/bin/openapi-mcp \
     --spec /tmp/scheduler-openapi-consumer.json \
     --base-url http://127.0.0.1:9090
```

Assertions (non-vacuous):

- exit 0; stderr contains `Generated 54 MCP tools` — **54**, matching the served operation count;
  a drift in either direction fails the test.
- The `id:2` response parses as JSON and `len(result.tools) == 54`.
- Spot-check the schema fidelity that muster's DF-034 path guarantees: the
  `delete_api_v1_projects_name` tool advertises required `["name","confirm"]` (confirm is a
  required query param on the route — a client that could not see it would 400 on every delete);
  `get_api_v1_projects` advertises optional `limit`/`offset`.

After SPEC-ROB-001, T1 runs **without the `jq` step**, with
`--spec http://127.0.0.1:9090/api/v1/openapi.json` — that swap is the proof the URL form works.

### T2 — a read round-trips through muster into the live service

Append to the probe:

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_api_v1_live","arguments":{}}}
```

Assertions:

- `result.isError` is absent/false, and `result.structuredContent.status == "ok"`, with
  non-empty `uptime` and a `build_sha`. (Measured live: the exact `{"build_sha":...,"status":"ok",
  "uptime":"26h50m...","version":...}` object came back through the MCP path.)
- **The healthy-route rule:** the read probe uses `/api/v1/live` (DB-free, SCHED-GAP-204-A) and —
  for a DB-backed second leg — `get_api_v1_queue` or `get_api_v1_metrics`, **not**
  `get_api_v1/status`. `/api/v1/status` is the SCHED-GAP-1575-family route: DB-backed, ~12
  sequential serialized calls, armed with a per-request 5s deadline (`--api-read-timeout`). It is
  healthy today (measured: 200 in 0.42s) and its failure mode is now a bounded 504 naming the
  stalled helper — but the acceptance test must not depend on the heaviest read to prove
  transport correctness. Status remains a legitimate muster tool; it is just not the acceptance
  vehicle.

### T3 — the mutation gate holds through the MCP path (negative cell, non-destructive, safe on the LIVE daemon)

Append:

```json
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"post_api_v1_pause","arguments":{}}}
```

Assertions (measured live — this exact probe returned):

- `result.isError == true` (a refusal is a well-formed MCP error result, never a hang),
- `result.structuredContent.error.code == "missing_credential"` with
  `class == "client_error"` — muster's 401/403 classification (DF-038) names the missing
  credential mechanism instead of relaying raw prose,
- the embedded upstream status is 401 with body
  `{"error":"operator credential required for mutations"}` (or 503 on a daemon with no
  credential configured — the fail-closed misconfiguration arm; accept either, assert
  `status ∈ {401, 503}`),
- **fleet-state non-vacuity:** `curl -sS http://127.0.0.1:9090/api/v1/health` afterwards shows
  the fleet NOT paused (ticks continue), and
  `curl -sS 'http://127.0.0.1:9090/api/v1/events?component=api.auth&limit=1'` contains the
  audited refusal row (`outcome` = `refused-unauthenticated`). If the gate were absent, this
  tool call would have returned 200 and PAUSED THE FLEET — the test fails loudly in that world.

### T4 — the operator-credentialed path works (positive mutation cell, SCRATCH daemon only — never :9090)

```sh
go build -o /tmp/schedulerd-muster-scratch ./cmd/schedulerd
mkdir -p /tmp/muster-scratch
head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > /tmp/muster-scratch/token; chmod 600 /tmp/muster-scratch/token
# The credential is ENV/TOML only — schedulerd REFUSES a CLI --operator-token flag
# by design (GAP-038: credentials in argv are visible in ps and shell history).
SCHEDULER_OPERATOR_TOKEN="$(cat /tmp/muster-scratch/token)" \
  /tmp/schedulerd-muster-scratch --db /tmp/muster-scratch/scheduler.db \
  --listen 127.0.0.1:19090 &
sleep 2
printf '%s\n%s\n%s\n%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
 '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"post_api_v1_pause","arguments":{}}}' \
 | MUSTER_AUTH_TOKEN="$(cat /tmp/muster-scratch/token)" ~/wojons/muster/bin/openapi-mcp \
     --spec /tmp/scheduler-openapi-consumer.json --base-url http://127.0.0.1:19090
```

Assertions (all verified live on a scratch daemon during this scoping pass): the
`id:3` call returns success (not `isError`) with `{"status":"paused"}`; the follow-up
`id:4` resume returns `{"status":"resumed"}`; the scratch daemon's
`/api/v1/events?component=api.auth` shows `allowed POST /api/v1/pause ... identity=operator:token`
and the matching resume row. This cell proves BOTH directions of
the credential story: with the credential, muster's process-level bearer reaches the daemon's
accepted `Authorization: Bearer` shape and the gate admits. The scratch instance also
exercises the fail-closed arm: a second probe run with `MUSTER_AUTH_TOKEN` unset must
get the 401 `refused-unauthenticated` arm (or 503 on a daemon launched with no
`SCHEDULER_OPERATOR_TOKEN` at all), never silent success. Kill the
scratch daemon and `rm -rf /tmp/muster-scratch` afterwards.

### T5 — an SSE tool call cannot wedge an agent (bounded-failure proof of the §1 prohibition)

```sh
printf '%s\n%s\n%s\n%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
 '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_api_v1_events_stream","arguments":{}}}' \
 | MUSTER_HTTP_TIMEOUT=5 ~/wojons/muster/bin/openapi-mcp \
     --spec /tmp/scheduler-openapi-consumer.json --base-url http://127.0.0.1:9090
```

Assertions: the whole pipeline **exits on its own**; the `id:3` response arrives as an error
result (timeout-class) in roughly the 5s budget, never an indefinite hang. This pins the §1/§4
claim that the SSE prohibition is enforced by a mechanical bound even when policy is ignored.

---

## 4 · MCP SAFETY BOUNDARY — agent-safe vs gated

**Muster contract referenced:** `muster/specs/PROTOCOLS.md` §4.5 ("MCP agent safety") — every
MCP-exposed operation returns or is bounded; boundaries are configured, never unlimited;
`KindEventSource` operations are never long-lived inside an MCP tool call; and (rule 3)
client-stream/bidi operations are not auto-exposed. The scheduler's surface maps onto that
contract as: 52 of 54 operations are unary request/response (naturally bounded); the 2 SSE GETs
are the event-source analogues, bounded mechanically by the executor's HTTP client timeout
(`MUSTER_HTTP_TIMEOUT`, default 30s) and forbidden by policy (§1, T5).

**The real boundary is the daemon's credential gate, and it is enforced under the MCP path
exactly as under REST** — proven live by T3. The served spec marks every gated operation with
`security: [{"operatorToken": []}]`. The complete gated list — **every one of these MUST stay
gated, and all are present (callable-by-operator, refused-for-agents) in the muster surface:**

| # | Operation | What it mutates |
|---|---|---|
| 1 | `POST /api/v1/pause` | fleet-wide scheduling pause |
| 2 | `POST /api/v1/resume` | fleet-wide scheduling resume |
| 3 | `POST /api/v1/evaluate` | forces a scheduling pass |
| 4 | `POST /api/v1/projects` | create project |
| 5 | `PUT /api/v1/projects/{name}` | full project field update (weights, models, prompts, enabled) |
| 6 | `DELETE /api/v1/projects/{name}` | soft/hard project delete (`confirm`, `purge`) |
| 7 | `POST /api/v1/projects/{name}/pause` | pause one lane |
| 8 | `POST /api/v1/projects/{name}/resume` | resume one lane |
| 9 | `POST /api/v1/projects/{name}/spawn` | force a worker spawn |
| 10 | `POST /api/v1/projects/{name}/bump` | priority bump |
| 11 | `POST /api/v1/projects/{name}/unbump` | clear bump |
| 12 | `POST /api/v1/namespaces` | create namespace |
| 13 | `PUT /api/v1/namespaces/{id}` | namespace update |
| 14 | `DELETE /api/v1/namespaces/{id}` | namespace delete (`confirm`/`purge`) |
| 15 | `POST /api/v1/namespaces/{id}/move` | move a project across namespaces |
| 16 | `POST /api/v1/groups` | create deploy group |
| 17 | `PUT /api/v1/groups/{name}` | update deploy group |
| 18 | `DELETE /api/v1/groups/{name}` | delete deploy group |
| 19 | `POST /api/v1/groups/{name}/deploy` | append template task rows to member boards |
| 20 | `POST /api/v1/templates` | create template |
| 21 | `PUT /api/v1/templates/{name}` | update template |
| 22 | `DELETE /api/v1/templates/{name}` | delete template |
| 23 | `GET /api/v1/peers` | gated READ — peer registry (anti-enumeration; SCHED-GAP-1602) |
| 24 | `POST /api/v1/peers` | register a federation peer |
| 25 | `POST /api/v1/peers/{id}/heartbeat` | peer heartbeat write |
| 26 | `POST /api/v1/federation/query` | query a peer (read-only by construction, still gated) |
| 27 | `GET /api/v1/federation/catalogue` | gated READ — federation catalogue |

**Agent-safe through MCP without any credential — the 27 open reads:** `GET` on `/api/v1/health`,
`/api/v1/live`, `/api/v1/status`, `/api/v1/config`, `/api/v1/queue`, `/api/v1/metrics`,
`/api/v1/events`, `/api/v1/events/stream`*, `/api/v1/observatory`, `/api/v1/observatory/stream`*,
`/api/v1/ticks`, `/api/v1/ticks/{id}`, `/api/v1/projects`, `/api/v1/projects/{name}`,
`/api/v1/lanes`, `/api/v1/lanes/tree`, `/api/v1/namespaces`, `/api/v1/namespaces/{id}`,
`/api/v1/namespaces/{id}/projects`, `/api/v1/groups`, `/api/v1/groups/{name}`,
`/api/v1/templates`, `/api/v1/templates/{name}`, `/api/v1/features`,
`/api/v1/features/prune-candidates`, `/api/v1/cooldown/residue`, `/api/v1/gateway-errors`.
(* = the two SSE routes: registered, but prohibited as MCP invocations per §1/T5; their poll
surrogates are the safe forms.) Open reads are doctrine, not oversight — internal/api/auth.go's
ROUTE SPLIT: "the mutation is the danger, not the observation," and the fleet shape is already
published off-box by the DuckBrain sync.

**Boundary mechanics pinned:**

- muster's executor sets the process-level `Authorization: Bearer` header **after** tool
  arguments (`pkg/mcp/registry.go`), so a generated header argument can never override or inject
  the configured credential. The scheduler, for its part, accepts credentials ONLY from request
  headers — never from arguments — so the two models compose with no injection seam.
- muster classifies upstream 401/403 as an authentication outcome with a
  `credentialSource` detail (`MUSTER_AUTH_TOKEN` / `--auth-token-file`), so an agent sees WHY it
  was refused, as structured content, with no credential material in the message.
- The scheduler's OWN built-in MCP server (`internal/mcp`) keeps its build-pinned
  `mutatingTools`/`readOnlyTools` classification (SCHED-GAP-1619). The muster path must NOT
  become a second authority: the served spec's `security` markers are the transport of the same
  single classification to OpenAPI consumers. Spec-row guard: keep the markers in lockstep with
  `mutatingTools` when routes change (both derive from the same route split).

---

## 5 · AUTH STORY — credential model vs muster's config surface

**Correcting the record first:** the brief's premise "no auth on :9090" is **stale**. Operator
auth landed (SCHED-GAP-1602, fail-closed mutation gate) and is LIVE: during this scoping pass the
running daemon answered an unauthenticated `POST /api/v1/pause` with
`401 {"error":"operator credential required for mutations"}` — i.e. the live host runs with a
token configured. A daemon with NO credential configured fails CLOSED instead: every mutation
answers `503` (there is nothing the caller could send), never silent admission.

**The scheduler's model** (internal/api/auth.go): one shared operator credential asserting "a
device the operator armed" — not a multi-tenant account system. Accepted shapes, constant-time
compared: `X-Operator-Token: <token>` header; `Authorization: Bearer <token>`; `Authorization:
Basic` with the token as the password; or full basic user+password (browser path, `[api]
operator_user/operator_password`). Token mode wins when both are configured. Every mutation
decision — allowed or refused — writes an audited events row (`component=api.auth`,
`refused-unauthenticated` / `refused-bad-credential` / `refused-no-credential` / `allowed`).
Reads are deliberately open under the loopback/tailnet threat model.

**What muster's config can carry** (cmd/mcp-server/main.go, pkg/mcp/registry.go): exactly ONE
process-level outbound bearer credential — `MUSTER_AUTH_TOKEN` (env) or `--auth-token-file
<path>` (flag, loaded once at startup, never logged or serialized, source recorded for error
classification). It is applied as `Authorization: Bearer <token>` on every outbound request. No
arbitrary-header knob, no per-tool credentials, no basic-auth client.

**Composition verdict — clean, with two gaps that don't matter:**

- `Authorization: Bearer` is one of the scheduler's accepted shapes, so the operator consumer
  (§2.1, `--auth-token-file`) works with zero scheduler-side changes. T4 proves it. (On the
  scheduler side the credential itself is env/TOML only — `SCHEDULER_OPERATOR_TOKEN` or
  `[api] operator_token` — never a CLI flag, by GAP-038.)
- Gap 1: `X-Operator-Token` direct-header mode is not reachable through muster. Doesn't matter —
  Bearer is an accepted equivalent; nothing needs the raw header form.
- Gap 2: basic user+password (browser) mode is not reachable through muster. Doesn't matter —
  token mode wins when both are configured, and the browser prompt is a separate seat anyway.

**Is the current posture acceptable for muster consumption? YES, and it is already the safest
configuration available:**

- Bind is `127.0.0.1:9090` only (default `--listen`; tailnet-forwarded via socat for the
  dashboard). Only same-host processes reach the port.
- An **agent consumer with no credential** (the §2.1 first block) gets exactly the 27 open reads
  and structured, audited refusals on all 27 gated operations. That is the §4 boundary, holding
  without any muster-side enforcement at all.
- An **operator consumer** adds `--auth-token-file` (0600 file). The credential never appears in
  the config JSON, argv-visible env, or muster's logs.

**What changes if exposure changes (triggers, written down now so the next decision is
mechanical):**

1. **Any non-loopback bind** (`--listen 0.0.0.0`, a LAN address, or a wider tailnet exposure):
   the open-reads doctrine must be re-threat-modeled BEFORE the bind changes — the current
   posture publishes fleet shape to anyone on the wire. The credential model itself needs no
   change (mutations stay gated), but reads stop being "already published anyway" the moment the
   audience grows. Minimum bar for any non-loopback muster consumer: `--auth-token-file`
   configured, and the §3 acceptance re-run with T3 expecting 401 (not 503) on the LIVE daemon.
2. **TLS termination** (public URL / SCHED-GAP-1600's HTTPS-origin row): muster's executor is a
   plain `http.Client` — it speaks TLS but has no client-cert/mTLS config surface in the
   mcp-server path. A TLS exposure therefore terminates at a proxy in front of the daemon; the
   muster consumer block then points `--base-url` at the proxy. Credential handling is unchanged.
3. **Credential rotation:** rotate by replacing the token file and restarting the consumer
   (muster loads it once at startup). The daemon accepts the new token with no restart of its
   own.
4. **The consumer block is bind-scoped:** the `--base-url http://127.0.0.1:9090` in §2 is valid
   ONLY for the loopback bind. Any bind/exposure change re-derives the block; it is never edited
   in place.

**Known-defect notes folded into the decisions above:**

- **`/api/v1/status` timeout family (SCHED-GAP-1575):** acceptance uses `/api/v1/live` +
  `/queue`/`/metrics` (§3 T2); status stays a normal tool. Depth of the defense: the handler's
  per-request deadline (5s default) is SHORTER than muster's default client timeout (30s), so
  even a wedged status call surfaces as the daemon's own 504 `{error, helper, detail}` — a
  well-formed muster error result naming the stalled step — never a hung tool call.
- **DB-derived spec correctness when the DB wedges:** the served spec contains no DB-derived
  content AT ALL — it is a build-time byte-template constant (`openapiSpec` in
  internal/api/server_helpers.go, version-stamped via `version.Current()`), served by a handler
  that writes the constant with no DB access. The spec therefore stays byte-correct when the DB
  route wedges. (The dynamic `info.version` tracks the binary, which is exactly what a consumer
  wants.)
- **Generate the contract from the route table instead of the handlers?** Not required for
  muster v1, and not a blocker for the spec row. The spec is hand-maintained but already pinned
  from both directions by in-repo tests (`TestAPI_OpenAPI_Success`: path set == the documented
  route table, every POST/PUT carries a requestBody schema, every operation declares a 2xx; the
  endpoints.md parity test pins the doc table against live route registrations). Generation would
  be a larger refactor of the same guarantee; file it as its own row if the parity tests ever
  prove too weak. The ONE genuinely hand-maintained risk — a new mutating route landing without
  its `security` marker — degrades documentation only, because the gate lives in the handler
  (§1.2). Keep the review rule, skip the generator.

---

## 6 · What this freezes (for SCHED-GAP-1580)

1. Surface = served spec as-is, 54 operations, no operationIds, SSE tools registered-but-prohibited.
2. Consumer config = §2.1 target / §2.2 interim (SPEC-ROB-001 moves `security-note`).
3. Acceptance = §3 T1–T5 (T3 runnable against the live daemon; T4 scratch-only).
4. Safety = §4's 27-operation gated list; agent consumers carry NO credential; the gate is
   server-side and audited.
5. Auth = Bearer-over-muster composes today; the four exposure-change triggers in §5 are the
   re-decision checklist.

No code, spec, binding, or board state was changed by this document.

<!-- ch:trace
STATUS: / TIER: T3 / EVIDENCE: docs/muster-scope.md committed
-->
