# Coding Hermes Fleet — Live Status

**Generated 2026-08-19 09:26 UTC from the live schedulerd API** (`GET http://127.0.0.1:9090/api/v1/status` + `/api/v1/projects`). Do not edit by hand — run `python3 docs/regenerate_fleet.py` to refresh.

## Settings (live)

| Setting | Value |
|---------|-------|
| Active projects (enabled) | 44 |
| Total projects (incl. disabled) | 72 |
| Active ticks | 1 |
| Budget | 100 |
| Last evaluation | 2026-08-19T09:25:30Z |
| Recent outcomes | completed=15383, failed=38846, timeout=4089 |
| DuckBrain sync | reachable=True, spooled_pending=0 |

## Fleet (72 projects, 44 enabled)

### Enabled (44)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| 9router | 10 | 15 | 3600s | coding-hermes |
| ai-plays-poke | 10 | 15 | 21600s | coding-hermes |
| asce | 10 | 15 | 21600s | coding-hermes |
| bunker | 10 | 15 | 3600s | coding-hermes |
| chimera-v2 | 10 | 15 | 21600s | coding-hermes |
| coding-hermes-scheduler | 10 | 15 | 3600s | coding-hermes |
| consensus | 10 | 15 | 3600s | coding-hermes |
| crier | 10 | 15 | 21600s | coding-hermes |
| deepseek-dashboard | 10 | 15 | 3600s | coding-hermes |
| dexdat-memory | 10 | 10 | 3600s | coding-hermes |
| duckbrain | 10 | 10 | 3600s | coding-hermes |
| h3 | 10 | 15 | 21600s | coding-hermes |
| h3-sdk-python-foreman | 10 | 15 | 3600s | coding-hermes |
| h3-shim-foreman | 10 | 15 | 21600s | coding-hermes |
| helix | 10 | 10 | 21600s | coding-hermes |
| hermes-canopy | 10 | 10 | 3600s | coding-hermes |
| hivemind-work | 10 | 15 | 21600s | coding-hermes |
| Kobayashi-Maru | 10 | 15 | 21600s | coding-hermes |
| muster | 10 | 15 | 21600s | coding-hermes |
| musterflow | 10 | 15 | 21600s | coding-hermes |
| rabbit-hole | 10 | 15 | 3600s | coding-hermes |
| ring-runner | 10 | 15 | 21600s | coding-hermes |
| speclang | 10 | 15 | 21600s | coding-hermes |
| terminal-jail | 10 | 15 | 21600s | coding-hermes |
| totalstack | 10 | 15 | 21600s | coding-hermes |
| warpfs | 10 | 15 | 43200s | coding-hermes |
| wojons-mythos | 10 | 15 | 21600s | coding-hermes |
| uhlp | 9 | 15 | 21600s | coding-hermes |
| gitreins-poc | 8 | 15 | 21600s | coding-hermes |
| h3-sdk-go-foreman | 8 | 10 | 3600s | coding-hermes |
| helios | 8 | 10 | 3600s | coding-hermes |
| hermes-dagger | 8 | 10 | 3600s | coding-hermes |
| inference-estimator | 8 | 10 | 3600s | coding-hermes |
| mafia-ai-benchmark | 8 | 10 | 21600s | coding-hermes |
| dexdat-core | 5 | 10 | 3600s | coding-hermes |
| eduos.dexdat.com.co | 5 | 10 | 21600s | coding-hermes |
| escalation-doctrine | 5 | 10 | 21600s | coding-hermes |
| h3-sdk-typescript-foreman | 5 | 10 | 3600s | coding-hermes |
| heading | 5 | 10 | 21600s | coding-hermes |
| hermes4friends-infra | 5 | 10 | 21600s | coding-hermes |
| imhotep | 5 | 10 | 3600s | coding-hermes |
| my-project | 5 | 10 | 21600s | coding-hermes |
| off-by-one | 5 | 10 | 21600s | coding-hermes |
| rethinkdb | 5 | 10 | 21600s | coding-hermes |

### Disabled (28)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
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
| zz-gap12-probe | 5 | 10 | 900s | - |
| zz-schedgap-011-probe | 5 | 10 | 900s | - |

## Live Dashboard

Point a browser at http://127.0.0.1:9090/ for the live HTML dashboard (auto-refreshes; per-project detail, queue, tick history, health).
