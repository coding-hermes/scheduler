# Coding Hermes Fleet — Live Status

**Generated 2026-09-01 01:02 UTC from the live schedulerd API** (`GET http://127.0.0.1:9090/api/v1/status` + `/api/v1/projects`). Do not edit by hand — run `python3 docs/regenerate_fleet.py` to refresh.

## Settings (live)

| Setting | Value |
|---------|-------|
| Active projects (enabled) | 91 |
| Total projects (incl. disabled) | 122 |
| Active ticks | 5 |
| Budget | 100 |
| Last evaluation | 2026-09-01T01:00:13Z |
| Recent outcomes | completed=19377, failed=39093, timeout=4119 |
| DuckBrain sync | reachable=True, spooled_pending=0 |

## Fleet (122 projects, 91 enabled)

### Enabled (91)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| 9router | 10 | 15 | 21600s | coding-hermes |
| ai-plays-poke | 10 | 15 | 21600s | coding-hermes |
| asce | 10 | 15 | 21600s | coding-hermes |
| bunker | 10 | 15 | 3600s | coding-hermes |
| chimera-v2 | 10 | 15 | 21600s | coding-hermes |
| coding-hermes-scheduler | 10 | 15 | 3600s | coding-hermes |
| consensus | 10 | 15 | 21600s | coding-hermes |
| crier | 10 | 15 | 21600s | coding-hermes |
| deepseek-dashboard | 10 | 15 | 21600s | coding-hermes |
| dexdat-memory | 10 | 10 | 21600s | coding-hermes |
| duckbrain | 10 | 10 | 21600s | coding-hermes |
| h3 | 10 | 15 | 3600s | coding-hermes |
| h3-sdk-python-foreman | 10 | 15 | 43200s | coding-hermes |
| h3-shim-foreman | 10 | 15 | 43200s | coding-hermes |
| helix | 10 | 10 | 21600s | coding-hermes |
| hermes-canopy | 10 | 10 | 7200s | coding-hermes |
| hermes-dagger | 10 | 10 | 21600s | coding-hermes |
| hivemind-work | 10 | 15 | 21600s | coding-hermes |
| Kobayashi-Maru | 10 | 15 | 21600s | coding-hermes |
| muster | 10 | 15 | 3600s | coding-hermes |
| musterflow | 10 | 15 | 21600s | coding-hermes |
| rabbit-hole | 10 | 15 | 21600s | coding-hermes |
| ring-runner | 10 | 15 | 21600s | coding-hermes |
| speclang | 10 | 15 | 21600s | coding-hermes |
| temple-runner | 10 | 10 | 3600s | coding-hermes |
| terminal-jail | 10 | 15 | 21600s | coding-hermes |
| totalstack | 10 | 15 | 21600s | coding-hermes |
| warpfs | 10 | 15 | 21600s | coding-hermes |
| wojons-mythos | 10 | 15 | 21600s | coding-hermes |
| uhlp | 9 | 15 | 3600s | coding-hermes |
| gitreins-poc | 8 | 15 | 21600s | coding-hermes |
| h3-sdk-go-foreman | 8 | 10 | 3600s | coding-hermes |
| helios | 8 | 10 | 3600s | coding-hermes |
| inference-estimator | 8 | 10 | 21600s | coding-hermes |
| mafia-ai-benchmark | 8 | 10 | 3600s | coding-hermes |
| 9router-sync | 5 | 1 | 21600s | duckbrain-sync |
| ai-plays-poke-sync | 5 | 1 | 21600s | duckbrain-sync |
| asce-sync | 5 | 1 | 21600s | duckbrain-sync |
| axiom-sync | 5 | 1 | 21600s | duckbrain-sync |
| blog-sync | 5 | 1 | 21600s | duckbrain-sync |
| bunker-sync | 5 | 1 | 21600s | duckbrain-sync |
| chimera-v2-sync | 5 | 1 | 21600s | duckbrain-sync |
| coding-hermes-sync | 5 | 1 | 21600s | duckbrain-sync |
| consensus-sync | 5 | 1 | 21600s | duckbrain-sync |
| crier-sync | 5 | 1 | 21600s | duckbrain-sync |
| deepseek-dashboard-sync | 5 | 1 | 21600s | duckbrain-sync |
| deepseek-payg-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-core | 5 | 10 | 3600s | coding-hermes |
| dexdat-core-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-memory-sync | 5 | 1 | 21600s | duckbrain-sync |
| duckbrain-sync | 5 | 1 | 21600s | duckbrain-sync |
| eduos-sync | 5 | 1 | 21600s | duckbrain-sync |
| eduos.dexdat.com.co | 5 | 10 | 21600s | coding-hermes |
| escalation-doctrine | 5 | 10 | 21600s | coding-hermes |
| escalation-doctrine-sync | 5 | 1 | 21600s | duckbrain-sync |
| frontiers-ghost-sync | 5 | 1 | 21600s | duckbrain-sync |
| gitreins-sync | 5 | 1 | 21600s | duckbrain-sync |
| h3-sdk-typescript-foreman | 5 | 10 | 3600s | coding-hermes |
| h3-umbrella-sync | 5 | 1 | 21600s | duckbrain-sync |
| heading | 5 | 10 | 21600s | coding-hermes |
| heading-sync | 5 | 1 | 21600s | duckbrain-sync |
| helios-sync | 5 | 1 | 21600s | duckbrain-sync |
| helix-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-agent-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-canopy-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes4friends-infra | 5 | 10 | 21600s | coding-hermes |
| hermes4friends-infra-sync | 5 | 1 | 21600s | duckbrain-sync |
| hivemind-sync | 5 | 1 | 21600s | duckbrain-sync |
| imhotep | 5 | 10 | 21600s | coding-hermes |
| inference-estimator-sync | 5 | 1 | 21600s | duckbrain-sync |
| kobayashi-maru-sync | 5 | 1 | 21600s | duckbrain-sync |
| mafia-benchmark-sync | 5 | 1 | 21600s | duckbrain-sync |
| muster-sync | 5 | 1 | 21600s | duckbrain-sync |
| musterflow-sync | 5 | 1 | 21600s | duckbrain-sync |
| my-project | 5 | 10 | 21600s | coding-hermes |
| mythos-sync | 5 | 1 | 21600s | duckbrain-sync |
| off-by-one | 5 | 10 | 21600s | coding-hermes |
| off-by-one-sync | 5 | 1 | 21600s | duckbrain-sync |
| rabbit-hole-sync | 5 | 1 | 21600s | duckbrain-sync |
| reports-sync | 5 | 1 | 21600s | duckbrain-sync |
| rethinkdb | 5 | 10 | 21600s | coding-hermes |
| rethinkdb-sync | 5 | 1 | 21600s | duckbrain-sync |
| ring-runner-sync | 5 | 1 | 21600s | duckbrain-sync |
| speclang-sync | 5 | 1 | 21600s | duckbrain-sync |
| task-router-sync | 5 | 1 | 21600s | duckbrain-sync |
| temple-runner-sync | 5 | 1 | 21600s | duckbrain-sync |
| temporal-vector-index-sync | 5 | 1 | 21600s | duckbrain-sync |
| terminal-jail-sync | 5 | 1 | 21600s | duckbrain-sync |
| totalstack-sync | 5 | 1 | 21600s | duckbrain-sync |
| uhlp-sync | 5 | 1 | 21600s | duckbrain-sync |
| warpfs-sync | 5 | 1 | 21600s | duckbrain-sync |

### Disabled (31)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| bankai | 10 | 10 | 21600s | coding-hermes |
| bankai-sync | 5 | 1 | 21600s | duckbrain-sync |
| ch-alpha | 9 | 35 | 43200s | test-dummy |
| ch-beta | 8 | 25 | 43200s | test-dummy |
| ch-delta | 6 | 5 | 43200s | test-dummy |
| ch-epsilon | 5 | 5 | 43200s | test-dummy |
| ch-eta | 2 | 5 | 43200s | test-dummy |
| ch-gamma | 7 | 10 | 43200s | test-dummy |
| ch-zeta | 4 | 5 | 43200s | test-dummy |
| dc-prune | 7 | 8 | 43200s | test-dummy |
| dc-rotate | 3 | 3 | 43200s | test-dummy |
| dc-vacuum | 5 | 5 | 43200s | test-dummy |
| dogfood-20260815 | 1 | 3 | 900s | - |
| dogfood-20260815-dup | 5 | 10 | 900s | - |
| dogfood-20260815-guard | 5 | 10 | 900s | - |
| global-fast | 10 | 15 | 43200s | test-dummy |
| global-slow | 1 | 10 | 43200s | test-dummy |
| HEADING | 10 | 25 | 43200s | coding-hermes |
| hivemind-pulse | 10 | 15 | 43200s | coding-hermes |
| mon-alert | 4 | 5 | 43200s | test-dummy |
| mon-check | 6 | 5 | 43200s | test-dummy |
| mon-ping | 8 | 10 | 43200s | test-dummy |
| mythos | 5 | 10 | 900s | coding-hermes |
| sim-alpha | 5 | 10 | 43200s | test-dummy |
| sim-beta | 8 | 20 | 43200s | test-dummy |
| sim-delta | 9 | 25 | 43200s | test-dummy |
| sim-gamma | 3 | 15 | 43200s | test-dummy |
| SpecLang | 10 | 15 | 43200s | coding-hermes |
| task-router | 10 | 10 | 21600s | coding-hermes |
| zz-gap12-probe | 5 | 10 | 900s | - |
| zz-schedgap-011-probe | 5 | 10 | 900s | - |

## Live Dashboard

Point a browser at http://127.0.0.1:9090/ for the live HTML dashboard (auto-refreshes; per-project detail, queue, tick history, health).
