# muster consumer wiring

[muster](https://github.com/wojons/muster) turns an OpenAPI 3.x document into a typed CLI
(`openapi-cli`), an MCP server (`openapi-mcp`), a daemon and a Go library. The scheduler's
contract for it is the **served** document — `GET /api/v1/openapi.json`, 45 paths / 58
operations, every operation with an `operationId` — documented in
[openapi.md](openapi.md). Nothing scheduler-side is muster-specific: same routes, same
credential gate, same audit log.

## Consumer config (copy-paste)

Agent consumer — no credential, exactly the open read set (mutations answer a structured
refusal):

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

Operator consumer — the credential comes from a `0600` file, never inline in a synced config:

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

```sh
openapi-cli discover http://127.0.0.1:9090/api/v1/openapi.json   # every operation, grouped, with its operationId
openapi-cli get-api-v1-live                                       # a read round-trips into the daemon (no flags — servers[] is absolute)
openapi-cli get-api-v1-projects                                   # the project list
```

Note: `--base-url` is optional everywhere above — the spec's `servers[0].url` is the
absolute `http://127.0.0.1:9090`, and muster falls back to it when the flag is omitted
(override it only with `--base-url` or `MUSTER_BASE_URL`).

## Quickstart

```sh
openapi-mcp --spec http://127.0.0.1:9090/api/v1/openapi.json --base-url http://127.0.0.1:9090   # MCP: "Generated 58 MCP tools"
openapi-cli generate http://127.0.0.1:9090/api/v1/openapi.json -u http://127.0.0.1:9090           # CLI: persist one verb per operation
openapi-cli get-api-v1-live                                                                      # drives the live daemon
```

## CLI form

Each operation becomes one top-level verb — the **kebab-case of its `operationId`**
(`getApiV1Live` → `get-api-v1-live`, `postApiV1ProjectsNamePause` →
`post-api-v1-projects-name-pause`):

```sh
openapi-cli get-api-v1-live                  # liveness (DB-free)
openapi-cli get-api-v1-cadence               # configured vs achieved starts/day per lane
openapi-cli get-api-v1-cooldown-residue      # cooldown-pin residue detector
```

**Base URL precedence, measured:** the verb's default host is the spec's absolute
`servers[].url` (`http://127.0.0.1:9090`, the daemon's default loopback bind), so a
default-address daemon needs no flags. Point it elsewhere at **runtime** with
`--base-url <url>` or `MUSTER_BASE_URL` — both override the spec, and a generate-time `-u`
does **not** stick to the persisted verb. A bind or port change is a `--base-url` change,
never a spec edit.

## What a consumer gets

- **58 MCP tools / 58 CLI verbs**, one per operation, named from the `operationId`.
- **The open read set** works with no credential: health, live, status, config, queue,
  metrics, events, ticks, projects, lanes, namespaces, groups, templates, features,
  cooldown residue, cadence, gateway errors, the Observatory snapshot and the document
  itself.
- **29 operations are operator-gated** (25 mutating + the peer/federation reads), marked
  per-operation with `security: [{"operatorToken": []}]` in the document. An agent calling
  one without a credential receives a well-formed, structured refusal (401, or 503 on a
  daemon with no credential configured — the fail-closed arm) and nothing is mutated. The
  full gated list is [muster-scope.md §4](../muster-scope.md).
- **The two SSE routes are registered but prohibited as MCP invocations**
  ([muster-scope.md §1/§4](../muster-scope.md)): `get-api-v1-events-stream` and
  `get-api-v1-observatory-stream` are long-lived streams, so steady-state consumers use the
  bounded poll surrogates `GET /api/v1/events?limit=N` and `GET /api/v1/observatory`. A
  mistaken stream call is bounded by the executor's HTTP client timeout (`MUSTER_HTTP_TIMEOUT`,
  default 30s) and returns as an error transcript rather than wedging an agent loop.
- **The tool count is a contract, not a constant guess:** the startup line is
  `Generated 58 MCP tools` for a tree at this document's shape. A different number means the
  surface moved — re-read [openapi.md](openapi.md), don't adjust the expectation.

Credentials: the scheduler accepts `Authorization: Bearer <token>` (muster's single
process-level form, via `MUSTER_AUTH_TOKEN` or `--auth-token-file`), which is one of its
accepted shapes — so the operator path composes with no scheduler-side change. The
credential is env/TOML only on the scheduler side (never argv, GAP-038) and never carried in
the consumer config. Details, exposure-change triggers and the audit model:
[muster-scope.md §5](../muster-scope.md).

## Verify the wiring

```sh
# 1. the document loads through muster's own strict validator (prints on stderr)
~/wojons/muster/bin/openapi-mcp --spec http://127.0.0.1:9090/api/v1/openapi.json \
  --base-url http://127.0.0.1:9090
#    → Loaded spec: Coding Hermes Scheduler API (v…)  /  Generated 58 MCP tools

# 2. a read round-trips through the generated surface into the live service
printf '%s\n%s\n%s\n' \
 '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}' \
 '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
 '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"getApiV1Live","arguments":{}}}' \
 | ~/wojons/muster/bin/openapi-mcp --spec http://127.0.0.1:9090/api/v1/openapi.json \
     --base-url http://127.0.0.1:9090
#    → result.structuredContent.status == "ok" with a build_sha and uptime

# 3. the document/route/documented-surface guards
go test -count=1 -run 'TestServedOpenAPISpecParity|TestAgentsEndpointTableParity' ./internal/mcp/
```

## References

- The contract itself: [openapi.md](openapi.md) · route table: [endpoints.md](endpoints.md)
- Scope decision (surface, boundary, auth, acceptance T1–T5): [docs/muster-scope.md](../muster-scope.md)
- Design: [docs/muster-integration.md](../muster-integration.md)

## Live proof (SCHED-GAP-1581)

Executed 2026-10-16/17 against the served contract. Raw outputs live in
`.coding-hermes/board/proofs/sched-gap-1581/` (`mcp-handshake.txt`, `cli-discover.txt`,
`cli-live.txt`, `cli-projects.txt`, `credential-arms.txt`). Muster used: **HEAD
`d95acc02`** (`v0.1.1-471-gd95acc02`, rebuilt from source with `make build` — the checked-out
`bin/` binaries predated the `d95acc02` tree; always rebuild before a proof).

**Read the daemon's build stamp before trusting a live result.** The production daemon on
127.0.0.1:9090 at proof time was built from pre-SCHED-GAP-1580 commit `33ceee01` (Oct 15 boot):
its `/api/v1/openapi.json` still served the OLD shape — 41 paths / 54 operations / **zero
`operationId`s** / the unprefixed `security-note` key — and the rebuilt `openapi-mcp` refuses
that document outright (`failed to load spec: validate spec: extra sibling fields:
[security-note]` — the SPEC-ROB-001 rejection SCHED-GAP-1580 fixed), while `openapi-cli
discover` prints the same surface **without verb names** and `openapi-cli get-api-v1-live`
round-trips (the CLI's generator is lenient; only the MCP server runs muster's strict
validator). The proof's contract target was therefore a scratch daemon built from this
tree (`schedulerd --listen 127.0.0.1:19081 --db /tmp/eval.db`, binary reports
`dev-88b441f9`) — every number below (45 paths / 58 ops / 58 ids / 29 markers +
`x-security-note`) matched the landed shape exactly. A production redeploy from main closes
the gap; until then the :9090 MCP consumer stays a zero-tool hard error, not a silent subset.

### MCP handshake (newline-delimited JSON-RPC 2.0 over stdio)

```sh
printf '%s\n%s\n%s\n' <initialize> <notifications/initialized> '<tools/list>' \
  | ~/wojons/muster/bin/openapi-mcp --spec http://127.0.0.1:19081/api/v1/openapi.json \
      --base-url http://127.0.0.1:19081
```

stderr:

```
Loaded spec: Coding Hermes Scheduler API (dev-88b441f9)
Generated 58 MCP tools
Using base URL: http://127.0.0.1:19081 (override=true)
```

`tools/list` returned exactly **58 tools** (full name list in the proof file); sample:
`getApiV1ProjectsName`, `putApiV1ProjectsName`, `getApiV1Peers`, `getApiV1TemplatesName`,
`getApiV1Features`, `getApiV1EventsStream`, `getApiV1Ticks`, `getApiV1Namespaces`. A
`tools/call` on `getApiV1Live` returned `result.structuredContent.status == "ok"` with the
daemon's real `build_sha: 88b441f9` — a read round-trips into the live service through MCP.

### CLI discover + read verbs

```sh
openapi-cli discover http://127.0.0.1:19081/api/v1/openapi.json   # 58 operations, all with verb names
openapi-cli generate http://127.0.0.1:19081/api/v1/openapi.json   # persisted the verb store (isolated run)
openapi-cli get-api-v1-projects --raw | jq '{total,first:.projects[0].name,repo:.projects[0].repo_url}'
#   → {"total":608,"first":"9router-docs","repo":"local:/home/kara/9router"}  (servers[] default → :9090)
openapi-cli get-api-v1-projects --base-url http://127.0.0.1:19081 --raw | jq '{total}'
#   → {"total":0}  (empty scratch DB — documented reality: --base-url overrides, the fleet DB does not follow)
```

The base-url precedence documented above held **live in both directions**:
`get-api-v1-live` with NO flags hit `:9090` (the spec's absolute `servers[]`) and returned
that daemon's `build_sha 33ceee01` / uptime 41h — and the same verb with
`--base-url http://127.0.0.1:19081` returned `build_sha 88b441f9` / uptime 44m. Both
responses are in `cli-live.txt` side by side. Full discover output (58 verbs + ids): the proof
dir's `cli-discover.txt`.

### Credential arms — both demonstrated

Mutating op used: `postApiV1Evaluate` → `POST /api/v1/evaluate` (`s.loop.ForceEvaluate()` —
forces the pass the daemon's own loop runs every tick; the lowest-stakes mutation on the
surface). Both arms' raw output is in the proof dir's `credential-arms.txt`.

**Operator arm (through muster, production daemon — credential configured):**

```sh
MUSTER_AUTH_TOKEN=$SCHEDULER_OPERATOR_TOKEN \
  openapi-cli post-api-v1-evaluate --base-url http://127.0.0.1:9090 --raw
#   → {"status":"evaluation triggered"}    200 OK
```

Audit record read back from the daemon itself (`GET /api/v1/events?component=api.auth`):

```
{"id":627183,"severity":"INFO","component":"api.auth",
 "message":"auth: allowed POST /api/v1/evaluate target=- identity=operator:token"}
```

**Fail-closed arm (daemon with NO credential configured):**

```
▪ curl -X POST → HTTP 503
  {"error":"mutations disabled: no operator credential configured (set SCHEDULER_OPERATOR_TOKEN / [api] operator_token)"}
▪ openapi-cli post-api-v1-evaluate --base-url http://127.0.0.1:19081
  error  mutations disabled: no operator credential configured …
  Error: HTTP error: 503 Service Unavailable
```

Both arm shapes match [openapi.md](openapi.md) "the boundary and its shape" verbatim; the
audit-readback closes the loop that the daemon saw the `operator:token` identity (not
"anonymous") on the allowed mutation.

### The withholder call

An agent consumer must NOT be given (unprompted) the **29 marker-gated operations** — the
25 mutating ops + these **4 gated reads**:

- `getApiV1Peers` — peer registry (anti-enumeration, SCHED-GAP-1602)
- `getApiV1PeersEvents` — aggregate federation event slice (REMOTE-006)
- `getApiV1PeersIdEvents` — per-peer federation event slice (REMOTE-006)
- `getApiV1FederationCatalogue` — federation catalogue

The contract withholds them by **two mechanisms composing** (see
[openapi.md](openapi.md): the shape table's "credential markers — 29 operations"
row, and the "Why there is no MCP-safe subset document" section for why the
boundary is server-side, not doc-side):

1. **Per-operation `security: [{"operatorToken": []}]` markers** in the served document —
   the transport of the route-split classification to OpenAPI consumers. All 29 are visible
   in `cli-discover.txt`'s discover output (every one prints its id but the agent has no
   credential); the marker is metadata-open, enforcement server-side.
2. **The server-side credential gate** (`internal/api/auth.go`, SCHED-GAP-1602) —
   `requireOperator` inside every mutating handler, fail-closed to 503 on unset credential,
   audited on both outcomes. An agent that guesses tokenless gets ARM 2 above, never a
   mutation; the structured refusal (DF-038) tells it WHY.

Note that muster-scope.md §4's 27-row gated table predates SCHED-GAP-1580's four new served
routes: of the current 29 markers, the 2 additions beyond the §4 table are the NEW
REMOTE-006 peer-events reads (`getApiV1PeersEvents` + `getApiV1PeersIdEvents`), marked and
enforced since 1580. The withholder call (agent gets only open reads + structured refusals;
operator token travels only in MUSTER_AUTH_TOKEN / --auth-token-file / request headers, never
argv or configs) is unchanged in discipline, not a drift.
