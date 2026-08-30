# SCHED-GAP-080 — Gateway 5xx spawn failures silently drop ticks (no retry)

**Priority:** P1 · **Date:** 2026-08-29 · **Status:** Root-caused, fix shipped

## Symptom

7 gateway spawn failures in the last 24h, each followed by a silent tick drop.
All failures are `HTTP 500: server_error: Internal server error: database disk
image is malformed` — the Hermes gateway's state.db was corrupted
(SCHED-GAP-079's scope to fix; this repo only schedules and must survive it).

## Log forensics (from ~/.hermes/coding-hermes/scheduler.log)

Exactly **7 `GATEWAY FAIL` pairs** (spawn.go:937 in the deployed binary), each
immediately followed by a **`SKIPPED`** line (spawn.go:955, "exec fallback
disabled, dropping tick"):

| # | Time (local) | GATEWAY FAIL (spawn.go:937) | SKIPPED (spawn.go:955) |
|---|--------------|------------------------------|------------------------|
| 1 | 2026/08/28 05:01:25 | ai-plays-poke-sync tick=ai-plays-poke-sync-2026-08-28-10-01-25 | same tick |
| 2 | 2026/08/28 05:01:40 | asce-sync tick=asce-sync-2026-08-28-10-01-30 | same tick |
| 3 | 2026/08/28 05:01:52 | axiom-sync tick=axiom-sync-2026-08-28-10-01-52 | same tick |
| 4 | 2026/08/28 05:02:24 | bankai-sync tick=bankai-sync-2026-08-28-10-01-57 | same tick |
| 5 | 2026/08/28 05:03:16 | bankai-sync tick=bankai-sync-2026-08-28-10-02-19 | same tick |
| 6 | 2026/08/28 05:03:28 | bunker-sync tick=bunker-sync-2026-08-28-10-02-31 | same tick |
| 7 | 2026/08/28 07:56:19 | heading-sync tick=heading-sync-2026-08-28-12-56-19 | same tick |

All 7 errors: `gateway POST: HTTP 500: server_error: Internal server error:
database disk image is malformed`. The 05:01–05:03 burst (6 ticks) coincides
with the gateway's state.db corruption window; the 07:56 drop (1 tick) is a
second corruption hit. Each drop = one scheduled tick that never ran.

## Root cause (code trace, CURRENT tree line refs)

1. **`internal/scheduler/gateway_client.go:217-224`** — `SendResponseWithSessionKey`
   classifies only 401/403 as `ErrGatewayKeyRejected` (GAP-035). Every other
   non-2xx (including 500) becomes a **plain error** (`gateway POST: HTTP 500: …`)
   with **no status-code structure** — the spawn path cannot distinguish a
   transient gateway 5xx from anything else.
2. **`internal/scheduler/spawn.go:817-885`** — the gateway dispatch closure
   retries **only** `ErrGatewayKeyRejected` chain hops (SCHED-GAP-064/065,
   `nextChainResolution`). A 5xx falls straight through the `err == nil ||
   !errors.Is(err, ErrGatewayKeyRejected) || !hasRetry` guard (line 823) and
   returns the error — **no transient retry exists**.
3. **`internal/scheduler/spawn.go:1023-1059`** — the error then hits
   `GATEWAY FAIL` log (line 1023), TASK-ROUTER-002 `recordCircuitFailure`
   (lines 1029-1031 — cools the router pair but never re-sends *this* tick),
   and — with `--no-exec-fallback` (default) — the `SKIPPED` block (lines
   1055-1059): `noteSpawnFailure` (spawn.go:168 → `consecutive_failures + 1`)
   then `return nil, …` → **the tick is dropped with no retry**.
4. Result: a transient gateway blip (500 for a few minutes) silently loses
   every tick it touches; the router pair is cooled for the *next* ticks but
   the current one is gone.

Deployed-binary line numbers (937/955) vs current tree (1023/1056) differ
because the running binary predates SCHED-GAP-079 additions; the code path is
identical.

## Fix design (SHIPPED in this commit)

1. **Transient-error classification** (`gateway_client.go`): new exported
   `GatewayStatusError{StatusCode, msg}` for non-2xx responses (error text
   byte-identical to legacy), `ErrGatewayTransient` sentinel for read/unmarshal
   failures, and `IsTransientGatewayErr(err)` — true for HTTP ≥500, network /
   timeout (`*url.Error`), read/unmarshal; **always false for 401/403**
   (`ErrGatewayKeyRejected` stays terminal, GAP-035, no retry flood).
2. **Bounded same-pair retry** (`spawn.go` dispatch closure): on
   `IsTransientGatewayErr`, retry the SAME model/provider pair (transport
   retry, NOT a chain hop — chain semantics untouched) up to
   `gatewayRetryMaxAttempts = 3` with exponential backoff
   (0.5s/1s/2s/4s-cap ≈ 3.5s worst case, ctx-bounded). Each attempt logs
   `GATEWAY RETRY: <project> tick=<id> attempt=k/3 …`. `recordCircuitFailure`
   fires only after exhaustion (existing GATEWAY FAIL → SKIPPED path unchanged,
   so the tick still fails with the gateway error persisted and
   `consecutive_failures` incremented — SCHED-GAP-079 completion gate intact).
3. **Observability** (`/api/v1/status` + `/api/v1/health`): `gateway_errors`
   counter (transient gateway spawn failures since restart; auth rejections
   never counted) alongside `spawns_http`/`spawns_exec`.

## Verification

- Forensics: `grep 'GATEWAY FAIL' scheduler.log | grep -E '2026/08/28 05:0[1-3]|07:56' | wc -l` → 7; same window `SKIPPED` → 7. Exact pairs listed above.
- New tests: `gateway_client_test.go` classifier tests (500→transient,
  401→terminal, timeout→transient, unmarshal→transient); `schedgap080_spawn_test.go`
  (blip-recovery: 500×1→200, tick completes, exactly 1 tick row, GATEWAY RETRY
  logged, no GATEWAY FAIL/SKIPPED; persistent-500: 4 POSTs, error persisted,
  consecutive_failures=1, no completed tick; 401: 1 POST, no retry, no counter;
  network-timeout recovery); `server_test.go` gateway_errors on /status + /health.
- `go build -o bin/schedulerd ./cmd/schedulerd/`, `go vet ./...`,
  `go test -short -p 1 ./...` all green (sequential per AGENTS.md).
