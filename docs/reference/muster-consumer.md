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
