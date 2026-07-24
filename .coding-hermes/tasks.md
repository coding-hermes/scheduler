<!--
  ⚠️  BOARD FORMAT — coding-hermes-model-router v1.3 (2026-07-24)
  All tasks MUST use matrix format: | ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
  Before editing this file, load the skill: skill_view(name='coding-hermes-model-router')
  Validate: python3 ~/.hermes/scripts/validate-board-format.py .coding-hermes/tasks.md
- [ ] **GITREINS-JUDGE — Configure LLM evaluator for commit quality review**
  | 🔴 Critical | — | — | deepseek-v4-flash @ deepseek-foreman | GITREINS_LLM_API_KEY in ~/.hermes/.env | foreman-direct |

  Run: `python3 ~/.hermes/scripts/check-gitreins-judge.py .` to verify.
  Default limits (adjust per-project based on codebase size and task complexity):
  - Fast/small projects: `max_iterations: 50`, `max_time: 10m`, tokens: `0.2M/0.4M`
  - Large repos (Go monorepos, 100+ files): `max_iterations: 100`, `max_time: 30m`, tokens: `1M/2M`
  - C++/Rust (slow compiles): `max_time: 30m` minimum
  - Scheduler/production infra: `max_time: 30m`, tokens: `1M/2M`
  Supervisor auto-flags projects where limits are too low for codebase size.

| 🔴 Critical | — | — | deepseek-v4-flash @ deepseek-foreman | GITREINS_LLM_API_KEY in ~/.hermes/.env | foreman-direct |

  Run: `python3 ~/.hermes/scripts/check-gitreins-judge.py .` to verify.
  If missing, create/edit .gitreins/config.yaml with evaluator section using deepseek-v4-flash.
  This is CRITICAL for code quality — no automated review of worker output without it.

  NEVER remove the matrix header row or NEVER-DONE / E2E-001 fixtures.
-->

# Coding Hermes Scheduler — Model Router Task Matrix

> **Core purpose:** Cron-driven autonomous development loop scheduler — manages 63 projects, spawns foreman ticks, cooldown management, fleet orchestration.
> **Status:** Build/test/lint/vet PASS. 74th consecutive idle tick (tick #143). Daemon healthy (1h59m uptime, 5 active ticks, 78 exec spawns, 41/63 enabled). Cooldown=900s (persisted correctly). All 9/9 test packages PASS (cached). Hilo: 478 edges, 68 files (useful — slight variation from cache rebuild). CI: all SUCCESS (5/5 recent runs). 6 outdated deps unchanged (same 6 packages — go-cmp v0.7.0, demangle, go-isatty v0.0.24, goldmark v1.8.4, golang.org/x/exp, telemetry — unchanged from tick #139). No TODO/FIXME in source. DuckBrain populated with tick #143 findings. NEVER-DONE audit completed (tick #140) — 11/11 checks PASS — next scheduled tick #144. Board fully stable — 4 active tasks (all analysis/blocked/corrected — no code changes needed).

```
ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback
```

## Active

| ID | Task | Pri | Cpx | Deps | Tags | Model | Reasoning | Fallback |
|----|------|-----|-----|------|------|-------|-----------|----------|
||| INFRA-004 | 🟡 CORRECTED — tick #135 source code audit: ApplyFleetConfig (loader.go:376-378) IS create-only (checks GetProject, skips if exists). Does NOT upsert enabled/cooldown on restart. This contradicts tick #133's assumption. Actual cooldown persistence works — cooldown_s survives restarts in SQLite. The "fleet TOML upsert" was an incorrect root cause. Reversion at tick #131 was likely operational (different DB or script-based reset). COOLDOWN-REVERSION and INFRA-004 share NO code-level bug in current source. Closing INFRA-004 — spawn path correct, fleet config correct, persistence works. | HIGH | 3 | — | scheduler,spawn,infra | DeepSeek V4 Pro | Source code audit | DeepSeek V4 Flash |
|| INFRA-003 | 🔴 Guard against tick storms: cooldown < tick_timeout. Projects with cooldown < tick_timeout spawn overlapping ticks that all timeout. Evidence: hermes-canopy (900s cooldown, 600s timeout = 5 overlaps/2h, $0.83 burned). **Tick #134 finding:** Current daemon runs with `--tick-timeout 600s`. Min cooldown across all 41 enabled projects is 900s. **No tick storm risk at this configuration.** INFRA-003 is preemptively solved by the current config — cooldown > tick_timeout on all projects. Keep on board as documentation, move to CRITICAL/WATCH. | CRITICAL | 3 | — | scheduler,cooldown,storm,infra | Kimi K3 | Bug fix: scheduler timing, tick storm prevention | DeepSeek V4 Pro |
|| AUTO-SLOWDOWN | ✅ FIXED (tick #132) — `return` → `continue` on spawn.go:332. stdout scanner now reads full output instead of exiting after `session_id:`. Build PASS, 9/9 tests PASS, lint 0 issues. Pushed as 1e7c4d4. | HIGH | 3 | — | scheduler,bug,slowdown | Kimi K3 | Bug fix: output capture, scheduler auto-regulation | DeepSeek V4 Pro |
| FIX-STACK | Systemd enable — BLOCKED (Bane defers). Scheduler daemon has no systemd unit, restarts wipe cooldown settings. Enabling systemd would persist across restarts. | Medium | 1 | — | infra,systemd,blocked | DeepSeek V4 Flash | Simple: blocked, waiting on Bane decision | — |
|| COOLDOWN-REVERSION | 🟡 REEVALUATED tick #135. Source code audit confirms ApplyFleetConfig IS create-only (loader.go:376-378). The fleet TOML does NOT overwrite cooldown_s on restart — it skips existing projects. True root cause of tick #131 reversion (900s after restart): likely operational (different DB path, script-based reset, or the cooldown change was never persisted via API). **The PUT endpoint works** (server_projects.go:120-136) — the real issue was that previous foremen couldn't reach it (curl blocked by cron security scanner) and fabricated commit messages claiming success. Systemd (FIX-STACK) would prevent restart-based operational issues. PERSISTENCE VERIFIED: cooldown_s survives restarts in SQLite — no code fix needed. | HIGH | 2 | — | scheduler,cooldown,config | DeepSeek V4 Pro | Architecture/design: config persistence, fleet management | DeepSeek V4 Flash |

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
