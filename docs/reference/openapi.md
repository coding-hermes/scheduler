# The served OpenAPI 3.x contract

**Status:** SCHED-GAP-1580 (spec row of the muster chain: [docs/muster-integration.md](../muster-integration.md) design → [docs/muster-scope.md](../muster-scope.md) scope → this → SCHED-GAP-1581 build).
Consumer wiring lives in [muster-consumer.md](muster-consumer.md).

## Where the contract lives

**Served at `GET /api/v1/openapi.json`** — OpenAPI **3.0.3**, JSON.

There is deliberately **no `docs/reference/openapi.yaml` file**: the served document is the single contract, and a committed copy would be a second one that rots on the first route change ([muster-scope.md §1.3](../muster-scope.md) rejects the forked-consumer-copy shape for the same reason). A consumer that needs a file can fetch the route and let its own tooling keep it — the document is stable, build-stamped and drift-guarded (below).

Source of truth: the byte constant `openapiSpec` in [`internal/api/server_helpers.go`](../../internal/api/server_helpers.go), written verbatim by the handler with **no DB access** — so the document stays correct even when the SQLite route is wedged. `info.version` is stamped from the binary (`version.Current()`), which is how a consumer detects a stale copy.

Current shape (measured on a daemon built from this tree — `/api/v1/openapi.json`, 2026-10-16):

| Property | Value |
|---|---|
| `openapi` | `3.0.3` |
| `paths` | **45** |
| operations | **58** (every one carrying an `operationId`) |
| `servers[]` | `[{"url": "http://127.0.0.1:9090", ...}]` — **absolute**, the daemon's default loopback bind |
| credential markers | **29** operations carry `security: [{"operatorToken": []}]` (25 mutating + the 4 gated federation/peer reads) |
| top-level keys | `openapi`, `info`, `servers`, `paths`, `components`, `x-security-note` |

## operationId rule (SCHED-GAP-1580)

Every operation carries an `operationId`, and the id is **derived, not authored**:

```
operationId = lowerCamel(method + full request path)
```

Split the path on `/`, brace-strip path parameters, split each segment again on `-`, `_` and `.`, capitalize the first letter of every part, concatenate, prefix the lower-case method.

| Route | operationId |
|---|---|
| `GET /api/v1/health` | `getApiV1Health` |
| `GET /api/v1/live` | `getApiV1Live` |
| `GET /api/v1/features/prune-candidates` | `getApiV1FeaturesPruneCandidates` |
| `GET /api/v1/openapi.json` | `getApiV1OpenapiJson` |
| `POST /api/v1/projects/{name}/pause` | `postApiV1ProjectsNamePause` |
| `DELETE /api/v1/projects/{name}` | `deleteApiV1ProjectsName` |
| `GET /api/v1/peers/{id}/events` | `getApiV1PeersIdEvents` |

Why derived rather than hand-chosen: a muster consumer names its CLI verbs and MCP tools from the `operationId`, so the id is contract, not a nicety. Two forms were available — semantic ids (`pauseProject`, the shape [docs/muster-integration.md §5](../muster-integration.md) sketched and the shape the in-fleet boardctl precedent uses) or the derived form above. The derived form is the one this repo can **verify**: boardctl derives its ids from a route registry that it generates the whole spec from, while this repo's document is a hand-maintained constant, so a hand-authored id list would be one more artifact that drifts — exactly the failure mode the parity guard exists for. The derived ids are also the *same words* the unconsumed surface already produced (`get_api_v1_live` → `getApiV1Live`), so nothing a reader of the scope document learned is invalidated.

If the owner prefers semantic ids, that is now a deliberate rename of 58 ids in one constant — i.e. exactly the breaking change [muster-scope.md §1](../muster-scope.md) warns about, and the reason they land **before** v1 instead of after a consumer exists.

## Why there is no "MCP-safe subset" document (deltas considered)

The spec row's original brief asked for `docs/reference/openapi-mcp-safe.json` — the served
spec filtered to a read-mostly subset, produced by a generation test that fails on drift from
an approved operationId list. The scope row ([muster-scope.md §1](../muster-scope.md)) explicitly
decided **against** this shape, for reasons that outweighed the brief's framing:

- The safety boundary is **server-side** — `requireOperator` refuses every mutating operation
  inside the handler (SCHED-GAP-1602), fail-closed, audited, regardless of what any client
  believes the spec says. Trimming the document changes documentation, not enforcement.
- A filtered copy is a **second classification** that drifts from the first (the same reason
  the scope row rejects any forked consumer doc), and hiding mutating operations from the
  document only hides the structured refusal an agent needs to see when it calls one.
- A subset-generation test would pin a second approved-operationId list against the served
  spec — the drift surface the served-spec parity guard (below) already pins against the
  actual route table. Two lists where one suffices.

The MCP boundary is expressed as the per-operation `security` markers in the served document
(the transport of `internal/api/auth.go`'s single route split) + muster's structured refusal
classification (DF-038: an agent sees *why* it is refused, as content) — verified live in
[muster-scope.md §3 T3](../muster-scope.md). Acceptance for the boundary claim is
[muster-scope.md §6 item 4](../muster-scope.md), not a subset file's existence.

## Generation rule: hand-maintained, drift-guarded — not generated

The document is **hand-maintained** in the `openapiSpec` constant. It is *not* generated from the route table, and [muster-scope.md §5](../muster-scope.md) records that decision ("keep the review rule, skip the generator"): there is no route registry to generate from, the document is descriptive prose plus per-operation schemas, and generating it would be a refactor of the same guarantee rather than a new one.

What makes hand-maintenance safe is that **three executable guards pin it from every side**, and each runs in a plain `go test ./...`:

| Guard | Proves |
|---|---|
| `internal/mcp/agents_endpoint_parity_test.go::TestServedOpenAPISpecParity` (SCHED-GAP-1580) | Fetches the **served** document from a real `api.Server` over `httptest` and checks: every spec path matches a real `http.ServeMux` route (a path inside a trailing-slash subtree must additionally name its sub-resource in the api sources, so an invented nested path cannot hide behind a subtree pattern); every `/api/v1` route in [endpoints.md](endpoints.md) is described; every operation has a derived, unique `operationId`; `servers[]` is absolute; no top-level key outside the OpenAPI vocabulary + `x-` extensions; every mutating operation carries its credential marker and no ungated read does. `TestServedOpenAPISpecParity_SelfCheck` proves each of those 10 arms fails when violated |
| `internal/mcp/agents_endpoint_parity_test.go::TestAgentsEndpointTableParity` (SCHED-GAP-166) | [endpoints.md](endpoints.md) ↔ route registrations, in both directions — the transitive half of the check above |
| `internal/api/server_test.go::TestAPI_OpenAPI_Success` | Structure of the served JSON (parseable, every POST/PUT has a `requestBody` schema, every operation declares a 2xx) and its path set against a hand-maintained list |

The generator row stays available if the guards ever prove too weak; nothing here blocks it.

## What SCHED-GAP-1580 changed

1. **SPEC-ROB-001 closed — the document is now loadable at all.** It carried a non-standard top-level sibling, `"security-note"`, and kin-openapi's `doc.Validate` — which muster runs before it generates anything — rejects the *entire* document: `failed to load spec: validate spec: extra sibling fields: [security-note]`. The prose moved to `"x-security-note"` (vendor-extension space, which the validator accepts). Before the fix a muster consumer generated **zero** tools; now it loads 58.
2. **`operationId` added to all 58 operations** (rule above).
3. **Four served routes the document did not describe** were added — `/api/v1/cadence`, `/api/v1/peers/events`, `/api/v1/peers/{id}/events` and the document's own `/api/v1/openapi.json`. The first three were served *and* documented in [endpoints.md](endpoints.md) while being invisible to an OpenAPI consumer; the api package's own test could not catch that because it compares against a hand list. Reverting only `internal/api/server_helpers.go` to its pre-row state makes the new guard report **59 divergences** naming exactly these.

## Verify it yourself

```sh
# shape (a running daemon, or your own: schedulerd --listen 127.0.0.1:19081)
curl -sS http://127.0.0.1:9090/api/v1/openapi.json \
  | jq '{openapi, paths:(.paths|length), ops:([.paths[]|to_entries[]]|length),
         ids:([.paths[][]|.operationId]|length), servers, top:(keys)}'

# muster's own strict loader (empty stderr on failure; the load line is the check)
~/wojons/muster/bin/openapi-mcp --spec http://127.0.0.1:9090/api/v1/openapi.json \
  --base-url http://127.0.0.1:9090
# → Loaded spec: Coding Hermes Scheduler API (v…) / Generated 58 MCP tools

# the repo's guards
go test -count=1 -run 'TestServedOpenAPISpecParity|TestAgentsEndpointTableParity' ./internal/mcp/
go test -count=1 -run 'TestAPI_OpenAPI' ./internal/api/
```

## References

- Consumer wiring + quickstart: [muster-consumer.md](muster-consumer.md)
- Scope decision (surface, safety boundary, auth): [docs/muster-scope.md](../muster-scope.md)
- Design: [docs/muster-integration.md](../muster-integration.md)
- Per-endpoint reference the spec's descriptions track: [docs/api.md](../api.md) · route table: [endpoints.md](endpoints.md)
- Muster's design authority: `muster/specs/PROTOCOLS.md` (§5.3 frozen MCP tool names, §4.5 agent safety)
