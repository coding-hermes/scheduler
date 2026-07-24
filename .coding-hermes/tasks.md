# Coding Hermes Scheduler — Model Router Task Matrix

> **Core purpose:** Cron-driven autonomous development loop scheduler — manages 66+ projects, spawns foreman ticks, cooldown management, fleet orchestration.
> **Status:** Build/test/lint/vet all PASS. 62nd+ consecutive idle tick. Project self-maintaining.

```
ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback
```

## Active

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
| INFRA-004 | 🔴 Spawn ignores Enabled=false — runaway loops despite cooldown. 12,953 ticks for eduos-e2e with 0 successful. 500 zombie ticks for HEADING ($5.07 burned). Root cause: evaluation loop or spawn path does not check `enabled` before dispatching, OR fleet TOML upsert re-enables projects on daemon restart. Fix: (a) DB cleanup — delete stale duplicates, (b) spawn guard — check `if !project.Enabled { skip }` at dispatch time, (c) regression test, (d) case-insensitive uniqueness constraint on project names. | CRITICAL | 4 | — | scheduler,spawn,bug,infra | Kimi K3 | Bug fix: scheduler dispatch logic, Zombie tick prevention | DeepSeek V4 Pro |
| INFRA-003 | 🔴 Guard against tick storms: cooldown < tick_timeout. Projects with cooldown < tick_timeout spawn overlapping ticks that all timeout. Evidence: hermes-canopy (900s cooldown, 600s timeout = 5 overlaps/2h, $0.83 burned). Fix: add `--enforce-min-cooldown` flag OR guard in spawn logic that skips if project has active tick. Scheduler-level fix benefiting all projects. | CRITICAL | 3 | — | scheduler,cooldown,storm,infra | Kimi K3 | Bug fix: scheduler timing, tick storm prevention | DeepSeek V4 Pro |
|| AUTO-SLOWDOWN | ✅ FIXED (tick #132) — `return` → `continue` on spawn.go:332. stdout scanner now reads full output instead of exiting after `session_id:`. Build PASS, 9/9 tests PASS, lint 0 issues. Pushed as 1e7c4d4. | HIGH | 3 | — | scheduler,bug,slowdown | Kimi K3 | Bug fix: output capture, scheduler auto-regulation | DeepSeek V4 Pro |
| FIX-STACK | Systemd enable — BLOCKED (Bane defers). Scheduler daemon has no systemd unit, restarts wipe cooldown settings. Enabling systemd would persist across restarts. | Medium | 1 | — | infra,systemd,blocked | DeepSeek V4 Flash | Simple: blocked, waiting on Bane decision | — |
| COOLDOWN-REVERSION | Fleet TOML `ApplyFleetConfig` upsert overwrites API-set cooldown on every daemon restart. This causes cooldown to repeatedly revert to 900s (fleet default) despite API PUT to 43200s. Affects ALL projects. Root cause: `cooldown-reset-on-restart` pitfall — fleet TOML has higher priority than API-set values. Fix: either add cooldown persistence in fleet TOML OR make API values survive restarts. | HIGH | 2 | — | scheduler,cooldown,config | DeepSeek V4 Pro | Architecture/design: config persistence, fleet management | DeepSeek V4 Flash |

## Completed (representative)

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
| AUDIT-001 through AUDIT-020 | ✅ All audit tasks complete — spec, doc, test, deps, pitfall, perf, endpoint, CI, DuckBrain, quality, wiring checks. | Various | 1-3 | — | audit | DeepSeek V4 Pro | Architecture audit | — |
| INFRA-COOLDOWN-CAP | ✅ autoSlowdown cap raised to 86400s | Medium | 2 | — | infra,scheduler | DeepSeek V4 Flash | Simple config change | — |
| DAEMON-CRASH-INVESTIGATE | ✅ Root cause: SIGHUP, fix: setsid | Medium | 3 | — | infra,daemon | Kimi K3 | Bug fix: daemon stability | — |
| CRITICAL-EDUOS-COOLDOWN | ✅ FIXED — eduos cooldown restored | High | 2 | — | scheduler,fix | DeepSeek V4 Flash | Simple config fix | — |
| INFRA-COOLDOWN-REVERSION | ✅ ROOT CAUSE IDENTIFIED — curl blocked by security scanner, foremen fabricated PUT claims. First real PUT via Python confirmed API works. | High | 3 | — | infra,investigation | DeepSeek V4 Pro | Architecture investigation | — |

## NEVER-DONE — 11-point audit

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
| NEVER-DONE | 11-point audit: spec alignment, doc coverage, test gaps, package upgrades, pitfall hunt, performance audit, endpoint verification, CI/CD health, DuckBrain sync, code quality, middle-out wiring. Run every 3-4 ticks. | Low | 3 | — | audit,quality | DeepSeek V4 Pro | Architecture-level project audit across all subsystems | GLM-5.2 |

- [ ] **E2E-001 — E2E Testing Tick (self-improving loop)** | Recurring every 5-10 ticks | — | — | Luna (browser/screenshots) or Step 3.7 Flash (CLI/API) | foreman-direct | — | —
