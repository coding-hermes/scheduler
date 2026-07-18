# Coding Hermes Fleet — H3 Status

**Current as of 2026-07-18 16:50 CT.** Managed by Coding Hermes Scheduler (`schedulerd :9090`).

## Architecture (Post BUG-006/007)

```
schedulerd (Go daemon, :9090)
├── evaluate() returns in <1s
│   ├── Phase 1 (locked): cleanup + pick projects
│   └── Phase 2 (lock-free): fire into SlotPool
├── SlotPool
│   ├── Buffered channel semaphore (cap = maxConcurrent)
│   ├── Each project: goroutine → acquire slot → spawn gateway → release
│   ├── SlotFreed() channel → event-driven re-eval (no ticker wait)
│   └── Timeout: 900s per tick
├── Gateway: HTTP API (hermes gateway :8642)
│   ├── spawner.Spawn() → POST /v1/responses
│   ├── Auto-approve (cron_mode: auto)
│   └── Delivery: hermes send via gateway
└── Health: /api/v1/health (lock-free, BUG-006 resolved)
```

## Settings

| Setting | Value |
|---------|-------|
| Cooldown (all projects) | 900s (15 min) |
| Tick timeout | 900s |
| Budget | 100 |
| Max concurrent | 12 |
| Min eval interval | 60s (periodic) |
| Instant re-eval | SlotFreed channel (event-driven) |
| Namespace mode | On |
| Gateway | `http://127.0.0.1:8642` |
| Gateway auth | `API_SERVER_KEY` env var |

## Fleet (38 projects, 35 enabled)

### Enabled (35)

| Project | Priority | Weight | Cooldown | Last Tick |
|---------|----------|--------|----------|-----------|
| coding-hermes-scheduler | 10 | 50 | 900s | Active |
| h3 | 5 | 10 | 900s | Active |
| h3-sdk-go-foreman | 5 | 10 | 900s | Active |
| h3-sdk-python-foreman | 5 | 10 | 900s | Active |
| h3-sdk-typescript-foreman | 5 | 10 | 900s | Active |
| h3-shim-foreman | 5 | 10 | 900s | Active |
| speclang | 8 | 10 | 900s | Active |
| asce | 8 | 50 | 900s | Active |
| totalstack | 5 | 10 | 900s | Active |
| dexdat-core | 6 | 10 | 900s | Active |
| dexdat-memory | 6 | 10 | 900s | Active |
| bunker | 7 | 10 | 900s | Active |
| Kobayashi-Maru | 7 | 10 | 900s | Active |
| helios | 7 | 10 | 900s | Active |
| helix | 7 | 10 | 900s | Active |
| muster | 5 | 10 | 900s | Active |
| musterflow | 7 | 15 | 900s | Active |
| mythos | 6 | 10 | 900s | Active |
| off-by-one | 6 | 10 | 900s | Active |
| imhotep | 6 | 10 | 900s | Active |
| consensus | 5 | 10 | 900s | Active |
| chimera-v2 | 5 | 10 | 900s | Active |
| hermes-dagger | 5 | 10 | 900s | Active |
| gitreins-poc | 5 | 10 | 900s | Active |
| crier | 5 | 10 | 900s | Active |
| rabbit-hole | 5 | 10 | 900s | Active |
| heading | 5 | 10 | 900s | Active |
| deepseek-dashboard | 4 | 10 | 900s | Active |
| duckbrain | 5 | 10 | 900s | Active |
| eduos | 5 | 10 | 900s | Active |
| eduos-e2e | 5 | 10 | 3600s | Active |
| warpefs | 5 | 10 | 900s | Active |
| escalation-doctrine | 5 | 10 | 900s | Active |
| mafia-ai-benchmark | 5 | 10 | 900s | Active |
| hivemind-work | 8 | 10 | 900s | Active |
| ai-plays-poke | 5 | 10 | 900s | Active |

### Disabled (3)
- hivemind-pulse (duplicate of hivemind-work)
- sim-* projects (test dummies)
- heading (duplicate, uppercase HEADING disabled)

## Recent Bug Fixes

| Bug | Date | Description |
|-----|------|-------------|
| BUG-007 | 2026-07-18 | Sequential spawn → SlotPool concurrent semaphore |
| BUG-006 | 2026-07-18 | evaluate() lock split (health endpoint deadlock) |
| BUG-005 | 2026-07-18 | Packer in-memory RunningSet (duplicate spawn race) |
| BUG-004 | 2026-07-18 | Goroutine leak in active map cleanup |
| FEAT-003 | 2026-07-18 | HTTP Gateway API spawn (0 process overhead) |

## Delivery

All projects deliver to their respective Telegram threads via `hermes send` through the Hermes gateway. Delivery targets are set per-project in the scheduler database.

## Supervisor

Standalone cron job (`55afdcd33d7f`) monitors scheduler health, detects orphans, and manages escalation. All foreman cron jobs are permanently paused — only the scheduler manages ticks.
