# S10 — Testing Strategy

**Status:** Draft  
**Depends on:** S01, S02, S05  
**Pages target:** 3-4

---

## 1. Overview

The scheduler uses Go's standard `testing` package with a focus on integration-style tests against real SQLite databases (in-memory). Tests run sequentially (`-p 1`) due to cgroup pid limits in the fleet environment.

## 2. Test Architecture

```
Test Category       Target          Approach
─────────────────────────────────────────────────
Unit (database)     database/*      In-memory SQLite, test helpers
Unit (scheduler)    scheduler/*     Mock gateway, test DB, exec mock
Unit (sync)         sync/*          In-memory SQLite, DuckBrain mock
Integration         *.go root       End-to-end spawn flow
Benchmarks          scheduler/*     Go benchmarks on hot paths
```

## 3. Database Tests (`internal/database/`)

- **Pattern:** `newTestDB(t)` creates an in-memory SQLite store
- **Coverage:** 69.3% (namespace CRUD, project CRUD, tick lifecycle)
- **Key helpers:** `database_test.go` — shared test DB setup
- **Gaps (LOW):** `events.go`, `schema.go`, `migrations.go` — infrastructure code

## 4. Scheduler Tests (`internal/scheduler/`)

- **Pattern:** Mock `gateway_client` via `httptest.NewServer`
- **Coverage:** ~62% (deliver 85%, gateway_client 87-100%, slowdown 100%, regression)
- **Key tests:**
  - `deliver_test.go` — output delivery with `exec.Command` mocking
  - `gateway_client_test.go` — HTTP client with transport error + timeout paths
  - `slowdown_test.go` — auto-slowdown escalation chain (18 tests)
  - `regression_test.go` — 19 regression tests for fleet rules
- **Gaps (LOW):** `spawn.go`, `slot_pool.go`, `sim.go`, `lifecycle.go` — 17+ functions at 0%

## 5. Command-Entry Tests

- **`cmd/schedulerd/`** — 0% coverage, accepted gap (CLI entry point)
- **`cmd/migrate/`** — 0% coverage, accepted gap (one-shot utility)

## 6. Benchmarks

7 benchmarks across 3 hot paths:
- `BenchmarkUrgencyCalc` — urgency scoring throughput
- `BenchmarkPackSelect` — multi-pool packing performance
- `BenchmarkDeliverOutput` — output delivery pipeline

Run with: `go test -bench=. -benchmem ./internal/scheduler/`

## 7. Test Conventions

- Sequential execution: `go test -short -p 1 ./...`
- Short mode: `-short` skips slow integration tests
- All tests use `t.Parallel()` only for independent subtests within a package
- GitReins guards enforce: secrets scan → build → lint → tests before commit
- CI (GitHub Actions): `golangci-lint` + `go test` on every push

## 8. Known Gaps

| ID | Gap | Priority |
|---|---|---|
| AUDIT-010 | scheduler package 17+ functions at 0% | LOW |
| AUDIT-016 | cmd/schedulerd + cmd/migrate 0% | LOW |
| AUDIT-014 | N+1 query in dashboard collect() | LOW |
