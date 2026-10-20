# Dogfood — the MCP surface, driven as a real consumer

**Run:** tick `coding-hermes-scheduler-dogfood-2026-10-20-06-54-47`, **third angle** (run 3 of the tick).
Run 1 = `784fdd8a` (cadence surface + `PERF-002-R2`), `493d11c2`, `e68834f0` (+ `PERF-001-R2`); run 2 = `b9e5d4a4` (operator troubleshooting taxonomy + `PERF-001-R3`, `SCHED-GAP-1747/1748`, `DOC-22`).
**Target:** HEAD `e209c70b` (no product change since run 2), live daemon `v1.12.0-488-gd6e17be8`, 608 projects / 411 enabled, 71 MCP tools.
**Verdict:** ✅ **the credential posture is genuinely good** (fail-closed everywhere, one ladder for REST and MCP, no leaking `data` field) / 🟡 **the introspection edges are rough** (unknown tool name, an unadvertised required param, and a not-found policy that disagrees with REST).

---

## 1. Why this angle

`README` promises "Exposes control — REST API, MCP, dashboard, DuckBrain sync", and MCP is how *agents* drive the fleet. Counting board history against the live registry, **26 of 71 MCP tools had never been named on the board at all** (`agents_*`, `lane_*`, `namespaces_*`, `templates_*`, `features_*`, `observatory_snapshot`, `peers_events`, `config_get`, `queue_get`, `tick_get`, `project_unbump`, `fleet_lane_tree`, `fleet_evaluate`). Prior MCP dogfood coverage was the 09-25 pre-auth mutation probe (SCHED-GAP-1619) and the 10-17 `fed_*` peer semantics — neither drove this registry.

## 2. What was driven

A real JSON-RPC client over `POST /mcp` (`/tmp/df-mcp-1020/mcp_probe.py`, stdlib only): `initialize` handshake → `tools/list` → error taxonomy → missing-argument validation on 16 tools → not-found semantics on 6 → 15 read-mirror calls → the fail-closed credential ladder on 13 gated tools (twice each: no credential, wrong credential). **59 calls**, per-call HTTP status, JSON-RPC code/message, latency and payload size captured (`probe.tsv`, `probe.json`).

**Read-only by construction:** no credential was presented or minted, and every gated tool was probed either with MISSING arguments or on ids/pairs that cannot exist — so even if the gate had failed open, the call would have been a no-op (`fleet_evaluate` triggers a normal evaluation, harmless; `lane_link_remove`/`templates_delete`/`project_unbump` were aimed at names that do not exist). Nothing on the live fleet was mutated.

## 3. What held up

- **Handshake**: `initialize` → 200 with `protocolVersion` + `serverInfo` + `capabilities` in 2.1 ms; `tools/list` → 71 tools in 0.9 ms. `notifications/initialized` answers `-32601 Method not found`, which is honest for a request/response HTTP transport (a stdio client must not expect a reply).
- **Method-level taxonomy**: unknown method → `-32601 Method not found: tools/bogus`; missing/unparseable params → `-32602 Invalid params: unexpected end of JSON input`. Correct and specific.
- **The credential ladder (the part with a real security history)**: all **13/13** gated tools answered `-32001 "operator credential required for mutations"` with no `data` field, and both a wrong `Authorization: Bearer` and a wrong `X-Operator-Token` answered `-32001 "invalid operator credential"` — byte-for-byte the three-arm table in `docs/api.md` ("MCP credential presentation (SCHED-GAP-1619)"). The 09-25 pre-auth-mutation finding has not regressed.
- **Argument validation on read tools**: `lane_is_ancestor`, `lane_relations`, `namespaces_get`, `namespaces_projects`, `templates_get`, `tick_get` all answer a schema-conformant `"<param> is required"` (-32000) — no Go/JSON internals leak.
- **Not-found on lookups**: `tick_get`, `namespaces_get`, `templates_get`, `lane_relations` each answer a named not-found error (`tick not found: …`, `namespace not found: …`, `not found: template "…"`, `project not found: …`).
- **Read mirrors all serve**: cadence 114.8 KB, fleet_lane_tree 66.6 KB, namespaces_list 45.3 KB, queue_get 22.7 KB, cooldown_residue 14.7 KB, observatory_snapshot 6.9 KB, features_get 1.6 KB, config_get 805 B, templates_list 429 B, agents_list 26 B — the MCP read surface mirrors the REST surface and answers fast (§4).

## 4. What broke — three findings

### 4.1 `SCHED-GAP-1749` (P3) — an unknown TOOL NAME is answered by the mutation-credential gate

```
tools/call cadence            -> 200, ok
tools/call agents_list        -> 200, ok
tools/call cadence_typo       -> 200, {"error":{"code":-32001,"message":"operator credential required for mutations"}}
tools/call agents_list_typo   -> 200, same
tools/call nope_nope          -> 200, same
tools/call cadence_typo + wrong X-Operator-Token -> -32001 "invalid operator credential"
```

A typo of a *read* tool is diagnosed as an auth problem; the client retries with a credential and gets a different auth error. The docs' refusal table says `-32001` means one of three credential states — here it means "no such tool". The 13-tool ladder itself is correct; only the lookup-before-gate order is wrong (and if gate-before-lookup is deliberate non-disclosure, the message must not assert "mutations").

### 4.2 `SCHED-GAP-1750` (P3) — `peers_events` cannot be called in either advertised shape

The schema offers only an optional `id` and the description promises "Omit id for every scheduler ('*')". Measured:

| args | answer |
|---|---|
| `{}` | `-32000 "remote last event id: scheduler_id required"` |
| `{"id":"*"}` | `-32000 "unknown peer * — register it first (peers_register)"` |
| `{"id":"nope"}` | `-32000 "unknown peer nope — register it first"` (correct) |
| `{"scheduler_id":"nope"}` | `-32000 "remote last event id: scheduler_id required"` (unknown arg ignored) |

`scheduler_id` — the parameter the error names — is **not in the schema**, so no schema-driven client can satisfy it; the handler evidently wants the remote peer's registry value. The documented all-schedulers default is unimplemented. (REST context: `GET /api/v1/peers/events` is credential-gated, the MCP tool is open — consistent with the documented "every MCP introspection read is open" posture, recorded for the integrator comparing transports.)

### 4.3 `SCHED-GAP-1751` (P3) — unknown-lane reachability disagrees with REST and with its own sibling tool

Same bogus input, three surfaces:

```
MCP  lane_is_ancestor{nope-lane, nope-other} -> 200 {"is_ancestor":false,"hops":0,"path":[]}
REST /links/ancestor?lane=nope-lane&ancestor=nope-other -> 404 {"error":"unknown lane: nope-lane","code":"unknown_lane"}
MCP  lane_relations{nope-lane}               -> -32000 "project not found: nope-lane"
```

A confident `false` reads as "these lanes exist and are unrelated" — the answer an agent would act on. The REST twin and the sibling tool both refuse, and since REST already owns an `unknown_lane` shape, the MCP tool should reuse it. Secondary instance of the same class (weaker, because REST agrees with MCP there): `namespaces_projects{"id":"nope-ns"}` → 200 with `projects: []` while `namespaces_get` → "namespace not found".

**Explicit non-finding, recorded so nobody "fixes" it:** the lane link store is legitimately EMPTY on this fleet — `lane_relations{"lane":"h3-qa"}` and REST `/links/parents?lane=h3-qa` both return `parents: []` — because `projects.parent` (h3-qa → 'h3-foreman' in the DB) is not the SCHED-GAP-1700 link store. So `lane_is_ancestor(h3-qa, h3-foreman) == false` is CORRECT; only the unknown-lane case above is a defect.

## 5. Performance (Step 2b) — nothing slow enough to file

| MCP tool | wall | payload |
|---|---|---|
| `observatory_snapshot` (default / `window=24h`) | **126.3 / 109.6 ms** | 6.9 / 10.4 KB |
| `cadence` | 10.6 ms | 114.8 KB |
| `fleet_lane_tree` | 7.9 ms | 66.6 KB |
| `cooldown_residue` | 2.3 ms | 14.7 KB |
| `queue_get` | 1.5 ms | 22.7 KB |
| `features_get` / `namespaces_list` / `templates_list` / `config_get` / `agents_list` | 0.5–1.4 ms | 26 B – 45.3 KB |
| gated refusals (13 tools) | 6.2–8.1 ms (one 33.7 ms, one 65.2 ms outlier) | — |
| REST twins (`/config` 0.5, `/queue` 1.4, `/namespaces` 0.7, `/features` 0.6, `/cadence` 8.2, `/cooldown/residue` 1.5, `/links` 1.1 ms) | comparable | — |

`observatory_snapshot` is the only call above 100 ms and it computes a tick-history snapshot over ~90 k rows — noticeable, not painful, and it is a snapshot tool by design, so **no PERF row is warranted**; the MCP transport adds no measurable overhead over the REST twins (same order on every mirror). Note this pass did not touch the `/status` family, so the run-2 lock-starvation stall did not contaminate these numbers.

## 6. Verdict

✅ **the parts with a security history are sound** — one credential ladder, fail-closed, honest messages, no `data` leak, and the reads are fast. 🟡 **the edges an integrator meets first are not**: a typo'd tool name reads as an auth failure, one tool's advertised call shape cannot succeed, and one family answers "false" where the other transports answer "unknown". None of these corrupt data; all of them cost an unattended agent a wrong turn and a misleading trace.

**Friction count: 3** (unknown-tool auth misdiagnosis; `peers_events` uncallable-as-advertised; unknown-lane divergence).

## 7. Artifacts and traceability

- Rows: `SCHED-GAP-1749`, `SCHED-GAP-1750`, `SCHED-GAP-1751` (boardctl create; validate OK; board 665 → 668).
- Evidence: `/tmp/df-mcp-1020/` — `mcp_probe.py` (59-call driver), `probe.tsv` / `probe.json` (per-call http/code/message/wall/keys), `followups.py` + `followups.log` (unknown-tool, peers_events, namespaces/lane contrasts), `ancestor_check.py` (the non-finding above), `tool_schema.py` (registry dump).
- `ch:trace`: `row=SCHED-GAP-1749 spec=docs/api.md(MCP credential presentation, SCHED-GAP-1619 table) test=none:manual-procedure(MCP probe matrix §2) evidence=/tmp/df-mcp-1020/{probe.tsv,followups.log} witness=live:POST-/mcp-59-calls verdict=new-finding commit=none:row-not-dispatched`
- Read-only lane: no source, no scheduler state, no fleet config, no credential minted or presented, no repo visibility touched; the only writes are this file, the log entry and the three rows.
