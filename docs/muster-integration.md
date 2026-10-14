# Muster Protocol Integration — Design (SCHED-GAP-1578)

**Status:** DESIGN — 2026-10-06. No code changes in this row. This document defines
what "connecting over the muster protocol" means for coding-hermes-scheduler: the
read surface muster consumes, the control surface muster may drive, and the
credential model that gates it. Everything below describes the scheduler's EXISTING
API ([docs/api.md](api.md), [docs/reference/endpoints.md](reference/endpoints.md)) —
muster adds no scheduler endpoints, headers, or auth modes.

## 1. What "connecting over the muster protocol" means

Muster (the `github.com/wojons/muster` engine, with `musterflow` as the built
integration tool at `github.com/totalwindupflightsystems/musterflow`) turns an
OpenAPI 3.x specification into a typed CLI (`openapi-cli` verbs), an HTTP MCP
server (`openapi-mcp`), and a Starlark workflow engine. It is not a bespoke wire
protocol: **connecting over the muster protocol means a muster consumer loads the
scheduler's served OpenAPI document and drives the scheduler's existing REST API
through it.** The scheduler changes no code to be connected; it publishes the spec
and keeps it accurate.

The single integration artifact already exists and is served live:

- `GET /api/v1/openapi.json` — the OpenAPI 3.0.3 document generated from the
  `openapiSpec` constant in `internal/api/server_helpers.go`
  ([docs/api.md §13](api.md#13-openapi-spec)). Its `servers[0].url` is the
  absolute `http://127.0.0.1:9090`, so a locally connected muster instance works
  with no extra flags; remote consumers (tailnet) override the base URL
  (`--base-url` / `MUSTER_BASE_URL`) rather than editing the spec.

Connection recipe (musterflow CLI form):

```sh
musterflow connect http://127.0.0.1:9090/api/v1/openapi.json
musterflow auth add <connection-slug>    # type "bearer"; value = the operator token
# → every scheduler endpoint becomes a CLI subcommand, an MCP tool, and a
#   Starlark-callable step, authenticated per §4
```

Precedent: the boardctl repo already ships this exact pattern
(`docs/muster/quickstart.md` there) — spec in, verbs out, a parity gate proving
the spec matches the wiring. This row adapts that model to the scheduler; see §5
for the one scheduler-side gap.

## 2. What the scheduler exposes to muster (read-only)

Muster's read surface is the API's deliberately-open GET set (SCHED-GAP-1602
"read-open vs write-gated", [docs/api.md §0](api.md#0-authentication-sched-gap-1602)):
no credential is required, and none is expected — cron probes, the ops watchdog
and Observatory-style pullers already consume these unauthenticated. A muster
connection is just one more such consumer. The three surfaces this row is about:

| Muster reads | Route | Notes |
|---|---|---|
| Project list / detail | `GET /api/v1/projects`, `GET /api/v1/projects/{name}` | Lanes, weights, priorities, cooldowns, enable state, namespace membership ([docs/api.md §5](api.md#5-projects)) |
| Tick history | `GET /api/v1/ticks`, `GET /api/v1/ticks/{id}` | Paginated tick history and one-tick drill-down ([docs/api.md §7](api.md#7-ticks)) |
| Namespace allocation | `GET /api/v1/namespaces`, `GET /api/v1/namespaces/{id}` | Namespace rows, weights, caps, members ([docs/api.md §6](api.md#6-namespaces)) |

The rest of the open read set is equally available to muster workflows:
`GET /api/v1/health`, `/live`, `/status`, `/config`, `/queue`, `/metrics`,
`/events` (and the SSE stream `/events/stream` for push-style polling),
`/features`, `/cadence`, `/openapi.json`.

Two read-open boundaries to know:

- **Federation and peer GETs are NOT read-open.** Every route under
  `/api/v1/peers` and `/api/v1/federation/*` — including the GETs — is
  operator-credential gated (fleet topology is not public read material;
  [docs/api.md §14](api.md#14-federation-peer-registry-remote-003)). With no
  credential configured the whole surface fails closed with 503.
- What muster never sees: the SQLite database file, the HTML dashboard routes
  (the human surface — not in the OpenAPI document), and anything the daemon
  serves that the spec does not describe. Muster's surface is exactly the spec.

## 3. What muster can request from the scheduler (control plane)

Muster drives mutations exactly as any authenticated HTTP client would — same
routes, same guards, same audit. The operations an operator would delegate to a
muster workflow:

| Action | Route | Guard behavior inherited by muster |
|---|---|---|
| Pause / resume one project | `POST /api/v1/projects/{name}/pause` · `/resume` | Operator gate ([docs/api.md §5](api.md#5-projects)); audited |
| Adjust a project's cooldown | `PUT /api/v1/projects/{name}` with `{"cooldown_s": <seconds>}` | Partial update; validation guards apply (400 on `decay_rate <= 0`, CHECK ranges, admission-law conflicts SCHED-GAP-1696; 404 unknown project) |
| Spawn a tick out-of-band | `POST /api/v1/projects/{name}/spawn` | 202 `{"status":"spawned","project":…,"tick_id":…}`; a disabled project is refused 409; the returned `tick_id` always resolves |
| Bump / unbump priority | `POST /api/v1/projects/{name}/bump` · `/unbump` | Operator gate |
| Delete a project | `DELETE /api/v1/projects/{name}?confirm=true` (`&purge=true` hard-delete) | 409 while the project is enabled — pause first; the guard is not bypassable via muster |
| Fleet-wide pause / resume / re-evaluate | `POST /api/v1/pause` · `/resume` · `/evaluate` | [docs/api.md §12](api.md#12-fleet-wide-control) |
| Project / namespace CRUD | `POST /api/v1/projects`, `PUT`/`DELETE` on the detail routes, same for `/api/v1/namespaces…` | Same guards as above, per [docs/api.md §5](api.md#5-projects)–[§6](api.md#6-namespaces) |

Every allowed AND refused mutating call writes an `api.auth` audit row visible
via `GET /api/v1/events`, so muster-driven control is attributable like any
operator action. Muster workflows get no special exemption anywhere; in
particular the federation read allow-list (`[federation.allow."*"]` /
`[federation.allow."<caller>"]`, deny-all when unset) still governs
`/api/v1/federation/query` regardless of who the caller is
([docs/federation-query-spec.md](federation-query-spec.md)).

## 4. Auth model

The scheduler's existing model applies unchanged (SCHED-GAP-1602,
[docs/api.md §0](api.md#0-authentication-sched-gap-1602)). The answer to "shared
secret via env, or the gateway key?" is: **a shared operator secret delivered via
env/TOML — and explicitly NOT the gateway key.**

| Concern | Model |
|---|---|
| Credential | One shared operator token. Sources, top-down: `SCHEDULER_OPERATOR_TOKEN` env (whitespace-trimmed at boot) → `[api] operator_token` TOML → `[api] operator_user` + `[api] operator_password` (Basic mode, the browser path). Never a CLI flag — argv leaks via `ps` (GAP-038) |
| Not the gateway key | `gateway_key` / `gateway_url` on a project row are DISPATCH configuration (which credential ticks present to the Hermes gateway, SCHED-GAP-1712). They never authenticate the control API, and the operator token never signs dispatch |
| Header forms accepted | `Authorization: Bearer <token>` (preferred) · `Authorization: Basic <b64>` (user part ignored, token as password) · `X-Operator-Token: <token>`. Constant-time comparison; 401 carries `WWW-Authenticate: Basic realm="scheduler operator"` |
| Reads | Unauthenticated by design (read-open, §2); the daemon binds loopback (`--listen 127.0.0.1:9090`) and fleet deployments reach it over the tailnet — a deployment-boundary decision, not an oversight |
| No credential configured | Fail-closed: `authOff` mode answers 503 on EVERY mutating route before any handler runs, even when a credential is presented. Boot announces `Auth: FAIL-CLOSED — no operator credential configured` |
| Muster-side storage | `musterflow auth add <connection-slug>` with type `bearer` (or `apikey`); musterflow stores the value masked in its data dir (`~/.musterflow/credentials.yaml`) and injects `Authorization: Bearer <token>` on every request — which matches the scheduler's preferred form |
| Auditability | Every mutating decision (allowed or refused) writes an `api.auth` audit row; muster calls are attributable like any operator |

One operational constraint, verified in musterflow's injection path
(`internal/auth/manager.go` `InjectAuthHeader`): musterflow sends ONLY the Bearer
form — it cannot produce the scheduler's Basic-mode pair. A scheduler configured
in Basic mode (user+password, no token) will 401 every muster mutation. Configure
`SCHEDULER_OPERATOR_TOKEN` (token mode) on any daemon a muster consumer drives.

## 5. Known gap: operationIds in the served spec

Verified 2026-10-06: the served OpenAPI document (`internal/api/server_helpers.go`)
carries **zero `operationId` fields**. Muster generates one named CLI verb per
operationId and cannot name a verb for an unnamed operation (the boardctl muster
integration hit the same rule), so a muster consumer of today's spec gets a
degraded surface: the document parses and lists paths, but verbs are unnameable
until operationIds exist.

Fix (a small code change, out of scope for this design row): add an `operationId`
per operation in the `openapiSpec` constant — e.g. `listProjects`, `getProject`,
`pauseProject`, `spawnProject` — following the boardctl naming convention
(lowerCamel verb-per-route). This doc should be updated with the verb table when
that lands. Until then, muster consumers can still drive the API programmatically
(Starlark `http` steps or plain curl against the documented routes), just not via
generated per-endpoint verbs.

## 6. Non-goals

- No new scheduler endpoints, headers, or auth modes for muster specifically.
- No muster-generated code committed to this repo — the served spec is the only
  shared artifact.
- No muster-specific credential or per-caller scoping: a muster caller is an
  operator-bearer caller like any other, distinguishable only in the audit log.
- No schema-drift risk added: the OpenAPI document and the route table are
  already locked together (`internal/mcp/agents_endpoint_parity_test.go` keeps
  [docs/reference/endpoints.md](reference/endpoints.md) honest against the real
  route registrations; the spec is generated from the same source tree).

## 7. References

- REST API reference: [docs/api.md](api.md) — §0 authentication (SCHED-GAP-1602),
  §5 projects, §6 namespaces, §7 ticks, §12 fleet-wide control, §13 OpenAPI spec,
  §14 peer registry, §15 federation query
- Route table: [docs/reference/endpoints.md](reference/endpoints.md)
- Federation: [docs/remote-spec.md](remote-spec.md),
  [docs/federation-query-spec.md](federation-query-spec.md)
- Muster engine and tool: `github.com/wojons/muster` (engine),
  `github.com/totalwindupflightsystems/musterflow` (CLI/MCP/workflow integration;
  connection + auth commands per its docs/integration-guide.md)
- In-fleet precedent: coding-hermes-boardctl `docs/muster/quickstart.md`
  (spec-in/verbs-out pattern and the operationId rule)
