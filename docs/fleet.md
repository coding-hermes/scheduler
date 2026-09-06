# Coding Hermes Fleet — Live Status

**Generated 2026-09-06 20:32 UTC from the live schedulerd API** (`GET http://127.0.0.1:9090/api/v1/status` + `/api/v1/projects`). Do not edit by hand — run `python3 docs/regenerate_fleet.py` to refresh.

## Settings (live)

| Setting | Value |
|---------|-------|
| Active projects (enabled) | 177 |
| Total projects (incl. disabled) | 256 |
| Active ticks | 0 |
| Budget | 100 |
| Last evaluation | 2026-09-06T20:31:12Z |
| Recent outcomes | completed=22611, failed=39227, timeout=4136 |
| DuckBrain sync | reachable=True, spooled_pending=0 |

## Fleet (256 projects, 177 enabled)

### Enabled (177)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| 9router | 10 | 15 | 3600s | coding-hermes |
| ai-plays-poke | 10 | 15 | 21600s | coding-hermes |
| asce | 10 | 15 | 21600s | coding-hermes |
| bunker | 10 | 15 | 3600s | coding-hermes |
| chimera-v2 | 10 | 15 | 3600s | coding-hermes |
| coding-hermes-scheduler | 10 | 15 | 3600s | coding-hermes |
| consensus | 10 | 15 | 21600s | coding-hermes |
| crier | 10 | 15 | 21600s | coding-hermes |
| deepseek-dashboard | 10 | 15 | 21600s | coding-hermes |
| dexdat-memory | 10 | 10 | 21600s | coding-hermes |
| duckbrain | 10 | 10 | 3600s | coding-hermes |
| h3 | 10 | 15 | 43200s | coding-hermes |
| h3-sdk-python-foreman | 10 | 15 | 43200s | coding-hermes |
| h3-shim-foreman | 10 | 15 | 43200s | coding-hermes |
| helix | 10 | 10 | 21600s | coding-hermes |
| hermes-dagger | 10 | 10 | 7200s | coding-hermes |
| hivemind-work | 10 | 15 | 21600s | coding-hermes |
| Kobayashi-Maru | 10 | 15 | 3600s | coding-hermes |
| muster | 10 | 15 | 604800s | coding-hermes |
| musterflow | 10 | 15 | 21600s | coding-hermes |
| rabbit-hole | 10 | 15 | 3600s | coding-hermes |
| ring-runner | 10 | 15 | 21600s | coding-hermes |
| speclang | 10 | 15 | 21600s | coding-hermes |
| temple-runner | 10 | 10 | 604800s | coding-hermes |
| terminal-jail | 10 | 15 | 21600s | coding-hermes |
| totalstack | 10 | 15 | 21600s | coding-hermes |
| warpfs | 10 | 15 | 21600s | coding-hermes |
| wojons-mythos | 10 | 15 | 21600s | coding-hermes |
| uhlp | 9 | 15 | 3600s | coding-hermes |
| gitreins-poc | 8 | 15 | 21600s | coding-hermes |
| h3-sdk-go-foreman | 8 | 10 | 43200s | coding-hermes |
| helios | 8 | 10 | 3600s | coding-hermes |
| inference-estimator | 8 | 10 | 21600s | coding-hermes |
| mafia-ai-benchmark | 8 | 10 | 3600s | coding-hermes |
| 9router-dogfood | 5 | 3 | 259200s | pm |
| ai-plays-poke-dogfood | 5 | 3 | 259200s | pm |
| ai-plays-poke-sync | 5 | 1 | 21600s | duckbrain-sync |
| asce-dogfood | 5 | 3 | 259200s | pm |
| asce-sync | 5 | 1 | 21600s | duckbrain-sync |
| axiom-sync | 5 | 1 | 21600s | duckbrain-sync |
| blog-sync | 5 | 1 | 21600s | duckbrain-sync |
| boardctl | 5 | 3 | 3600s | coding-hermes |
| boardctl-sync | 5 | 1 | 21600s | duckbrain-sync |
| bunker-dogfood | 5 | 3 | 259200s | pm |
| bunker-sync | 5 | 1 | 21600s | duckbrain-sync |
| chimera-v2-dogfood | 5 | 3 | 259200s | pm |
| chimera-v2-sync | 5 | 1 | 21600s | duckbrain-sync |
| coding-hermes-sync | 5 | 1 | 21600s | duckbrain-sync |
| consensus-dogfood | 5 | 3 | 259200s | pm |
| consensus-sync | 5 | 1 | 21600s | duckbrain-sync |
| crier-dogfood | 5 | 3 | 259200s | pm |
| crier-sync | 5 | 1 | 21600s | duckbrain-sync |
| deepseek-dashboard-dogfood | 5 | 3 | 259200s | pm |
| deepseek-dashboard-sync | 5 | 1 | 21600s | duckbrain-sync |
| deepseek-payg-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-core | 5 | 10 | 3600s | coding-hermes |
| dexdat-core-dogfood | 5 | 3 | 259200s | pm |
| dexdat-core-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-memory-dogfood | 5 | 3 | 259200s | pm |
| dexdat-memory-sync | 5 | 1 | 21600s | duckbrain-sync |
| duckbrain-sync | 5 | 1 | 21600s | duckbrain-sync |
| eduos-sync | 5 | 1 | 21600s | duckbrain-sync |
| escalation-doctrine | 5 | 10 | 21600s | coding-hermes |
| escalation-doctrine-dogfood | 5 | 3 | 259200s | pm |
| escalation-doctrine-sync | 5 | 1 | 21600s | duckbrain-sync |
| frontiers-ghost-sync | 5 | 1 | 21600s | duckbrain-sync |
| gitreins-poc-dogfood | 5 | 3 | 259200s | pm |
| gitreins-sync | 5 | 1 | 21600s | duckbrain-sync |
| h3-dogfood | 5 | 3 | 259200s | pm |
| h3-sdk-go-foreman-dogfood | 5 | 3 | 259200s | pm |
| h3-sdk-python-foreman-dogfood | 5 | 3 | 259200s | pm |
| h3-sdk-typescript-foreman | 5 | 10 | 43200s | coding-hermes |
| h3-sdk-typescript-foreman-dogfood | 5 | 3 | 259200s | pm |
| h3-shim-foreman-dogfood | 5 | 3 | 259200s | pm |
| h3-umbrella-sync | 5 | 1 | 21600s | duckbrain-sync |
| heading | 5 | 10 | 3600s | coding-hermes |
| heading-dogfood | 5 | 3 | 259200s | pm |
| heading-sync | 5 | 1 | 21600s | duckbrain-sync |
| helios-dogfood | 5 | 3 | 259200s | pm |
| helios-sync | 5 | 1 | 21600s | duckbrain-sync |
| helix-dogfood | 5 | 3 | 259200s | pm |
| helix-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-agent-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-canopy-dogfood | 5 | 3 | 259200s | pm |
| hermes-canopy-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-dagger-dogfood | 5 | 3 | 259200s | pm |
| hermes4friends-infra | 5 | 10 | 21600s | coding-hermes |
| hermes4friends-infra-dogfood | 5 | 3 | 259200s | pm |
| hermes4friends-infra-sync | 5 | 1 | 21600s | duckbrain-sync |
| hivemind-sync | 5 | 1 | 21600s | duckbrain-sync |
| hivemind-work-dogfood | 5 | 3 | 259200s | pm |
| imhotep | 5 | 10 | 21600s | coding-hermes |
| imhotep-dogfood | 5 | 3 | 259200s | pm |
| inference-estimator-dogfood | 5 | 3 | 259200s | pm |
| inference-estimator-sync | 5 | 1 | 21600s | duckbrain-sync |
| Kobayashi-Maru-dogfood | 5 | 3 | 259200s | pm |
| kobayashi-maru-sync | 5 | 1 | 21600s | duckbrain-sync |
| mafia-ai-benchmark-dogfood | 5 | 3 | 259200s | pm |
| mafia-benchmark-sync | 5 | 1 | 21600s | duckbrain-sync |
| muster-dogfood | 5 | 3 | 604800s | pm |
| muster-sync | 5 | 1 | 21600s | duckbrain-sync |
| musterflow-dogfood | 5 | 3 | 259200s | pm |
| musterflow-sync | 5 | 1 | 21600s | duckbrain-sync |
| my-project | 5 | 10 | 21600s | coding-hermes |
| mythos-sync | 5 | 1 | 21600s | duckbrain-sync |
| off-by-one | 5 | 10 | 21600s | coding-hermes |
| off-by-one-dogfood | 5 | 3 | 259200s | pm |
| off-by-one-sync | 5 | 1 | 21600s | duckbrain-sync |
| rabbit-hole-dogfood | 5 | 3 | 259200s | pm |
| rabbit-hole-sync | 5 | 1 | 21600s | duckbrain-sync |
| reports-sync | 5 | 1 | 21600s | duckbrain-sync |
| rethinkdb | 5 | 10 | 21600s | coding-hermes |
| rethinkdb-dogfood | 5 | 3 | 259200s | pm |
| rethinkdb-sync | 5 | 1 | 21600s | duckbrain-sync |
| ring-runner-dogfood | 5 | 3 | 259200s | pm |
| ring-runner-sync | 5 | 1 | 21600s | duckbrain-sync |
| speclang-dogfood | 5 | 3 | 259200s | pm |
| speclang-sync | 5 | 1 | 21600s | duckbrain-sync |
| task-router-sync | 5 | 1 | 21600s | duckbrain-sync |
| temple-runner-dogfood | 5 | 3 | 604800s | pm |
| temple-runner-sync | 5 | 1 | 21600s | duckbrain-sync |
| temporal-vector-index-sync | 5 | 1 | 21600s | duckbrain-sync |
| terminal-jail-dogfood | 5 | 3 | 259200s | pm |
| terminal-jail-sync | 5 | 1 | 21600s | duckbrain-sync |
| totalstack-dogfood | 5 | 3 | 259200s | pm |
| totalstack-sync | 5 | 1 | 21600s | duckbrain-sync |
| uhlp-dogfood | 5 | 3 | 259200s | pm |
| uhlp-sync | 5 | 1 | 21600s | duckbrain-sync |
| warpfs-dogfood | 5 | 3 | 259200s | pm |
| warpfs-sync | 5 | 1 | 21600s | duckbrain-sync |
| wojons-mythos-dogfood | 5 | 3 | 259200s | pm |
| 9router-qa | 4 | 4 | 43200s | qa |
| ai-plays-poke-qa | 4 | 4 | 43200s | qa |
| asce-qa | 4 | 4 | 43200s | qa |
| bunker-qa | 4 | 4 | 43200s | qa |
| chimera-v2-qa | 4 | 4 | 43200s | qa |
| consensus-qa | 4 | 4 | 900s | qa |
| crier-qa | 4 | 4 | 43200s | qa |
| deepseek-dashboard-qa | 4 | 4 | 43200s | qa |
| dexdat-core-qa | 4 | 4 | 43200s | qa |
| dexdat-memory-qa | 4 | 4 | 43200s | qa |
| escalation-doctrine-qa | 4 | 4 | 43200s | qa |
| gitreins-poc-qa | 4 | 4 | 43200s | qa |
| h3-qa | 4 | 4 | 43200s | qa |
| h3-sdk-go-foreman-qa | 4 | 4 | 43200s | qa |
| h3-sdk-python-foreman-qa | 4 | 4 | 43200s | qa |
| h3-sdk-typescript-foreman-qa | 4 | 4 | 43200s | qa |
| h3-shim-foreman-qa | 4 | 4 | 43200s | qa |
| heading-qa | 4 | 4 | 43200s | qa |
| helios-qa | 4 | 4 | 43200s | qa |
| helix-qa | 4 | 4 | 43200s | qa |
| hermes-canopy-qa | 4 | 4 | 43200s | qa |
| hermes-dagger-qa | 4 | 4 | 43200s | qa |
| hermes4friends-infra-qa | 4 | 4 | 43200s | qa |
| hivemind-work-qa | 4 | 4 | 900s | qa |
| imhotep-qa | 4 | 4 | 43200s | qa |
| inference-estimator-qa | 4 | 4 | 43200s | qa |
| Kobayashi-Maru-qa | 4 | 4 | 43200s | qa |
| mafia-ai-benchmark-qa | 4 | 4 | 43200s | qa |
| muster-qa | 4 | 4 | 604800s | qa |
| musterflow-qa | 4 | 4 | 43200s | qa |
| off-by-one-qa | 4 | 4 | 43200s | qa |
| qa-audit | 4 | 5 | 86400s | qa |
| rabbit-hole-qa | 4 | 4 | 43200s | qa |
| rethinkdb-qa | 4 | 4 | 43200s | qa |
| ring-runner-qa | 4 | 4 | 43200s | qa |
| speclang-qa | 4 | 4 | 43200s | qa |
| temple-runner-qa | 4 | 4 | 604800s | qa |
| terminal-jail-qa | 4 | 4 | 43200s | qa |
| totalstack-qa | 4 | 4 | 43200s | qa |
| uhlp-qa | 4 | 4 | 43200s | qa |
| warpfs-qa | 4 | 4 | 43200s | qa |
| wojons-mythos-qa | 4 | 4 | 43200s | qa |
| coding-hermes-scheduler-pm | 3 | 3 | 86400s | pm |
| duckbrain-pm | 3 | 3 | 86400s | pm |
| my-project-pm | 3 | 3 | 86400s | pm |
| release-engineer | 3 | 5 | 604800s | releases |

### Disabled (79)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| 9router-pm | 3 | 3 | 86400s | pm |
| 9router-sync | 5 | 1 | 21600s | duckbrain-sync |
| ai-plays-poke-pm | 3 | 3 | 86400s | pm |
| asce-pm | 3 | 3 | 86400s | pm |
| bankai | 10 | 10 | 21600s | coding-hermes |
| bankai-sync | 5 | 1 | 21600s | duckbrain-sync |
| bunker-pm | 3 | 3 | 86400s | pm |
| ch-alpha | 9 | 35 | 43200s | test-dummy |
| ch-beta | 8 | 25 | 43200s | test-dummy |
| ch-delta | 6 | 5 | 43200s | test-dummy |
| ch-epsilon | 5 | 5 | 43200s | test-dummy |
| ch-eta | 2 | 5 | 43200s | test-dummy |
| ch-gamma | 7 | 10 | 43200s | test-dummy |
| ch-zeta | 4 | 5 | 43200s | test-dummy |
| chimera-v2-pm | 3 | 3 | 86400s | pm |
| consensus-pm | 3 | 3 | 86400s | pm |
| crier-pm | 3 | 3 | 86400s | pm |
| dc-prune | 7 | 8 | 43200s | test-dummy |
| dc-rotate | 3 | 3 | 43200s | test-dummy |
| dc-vacuum | 5 | 5 | 43200s | test-dummy |
| deepseek-dashboard-pm | 3 | 3 | 86400s | pm |
| dexdat-core-pm | 3 | 3 | 86400s | pm |
| dexdat-memory-pm | 3 | 3 | 86400s | pm |
| doc-writer | 2 | 3 | 604800s | doc-writer |
| dogfood-20260815 | 1 | 3 | 900s | - |
| dogfood-20260815-dup | 5 | 10 | 900s | - |
| dogfood-20260815-guard | 5 | 10 | 900s | - |
| eduos.dexdat.com.co | 5 | 10 | 21600s | coding-hermes |
| eduos.dexdat.com.co-dogfood | 5 | 3 | 259200s | pm |
| eduos.dexdat.com.co-pm | 3 | 3 | 86400s | pm |
| eduos.dexdat.com.co-qa | 4 | 4 | 43200s | qa |
| escalation-doctrine-pm | 3 | 3 | 86400s | pm |
| gitreins-poc-pm | 3 | 3 | 86400s | pm |
| global-fast | 10 | 15 | 43200s | test-dummy |
| global-slow | 1 | 10 | 43200s | test-dummy |
| h3-pm | 3 | 3 | 86400s | pm |
| h3-sdk-go-foreman-pm | 3 | 3 | 86400s | pm |
| h3-sdk-python-foreman-pm | 3 | 3 | 86400s | pm |
| h3-sdk-typescript-foreman-pm | 3 | 3 | 86400s | pm |
| h3-shim-foreman-pm | 3 | 3 | 86400s | pm |
| HEADING | 10 | 25 | 43200s | coding-hermes |
| heading-pm | 3 | 3 | 86400s | pm |
| helios-pm | 3 | 3 | 86400s | pm |
| helix-pm | 3 | 3 | 86400s | pm |
| hermes-canopy | 10 | 10 | 7200s | coding-hermes |
| hermes-canopy-pm | 3 | 3 | 86400s | pm |
| hermes-dagger-pm | 3 | 3 | 86400s | pm |
| hermes4friends-infra-pm | 3 | 3 | 86400s | pm |
| hivemind-pulse | 10 | 15 | 43200s | coding-hermes |
| hivemind-work-pm | 3 | 3 | 86400s | pm |
| imhotep-pm | 3 | 3 | 86400s | pm |
| inference-estimator-pm | 3 | 3 | 86400s | pm |
| Kobayashi-Maru-pm | 3 | 3 | 86400s | pm |
| mafia-ai-benchmark-pm | 3 | 3 | 86400s | pm |
| mon-alert | 4 | 5 | 43200s | test-dummy |
| mon-check | 6 | 5 | 43200s | test-dummy |
| mon-ping | 8 | 10 | 43200s | test-dummy |
| muster-pm | 3 | 3 | 86400s | pm |
| musterflow-pm | 3 | 3 | 86400s | pm |
| mythos | 5 | 10 | 900s | coding-hermes |
| off-by-one-pm | 3 | 3 | 86400s | pm |
| rabbit-hole-pm | 3 | 3 | 86400s | pm |
| rethinkdb-pm | 3 | 3 | 86400s | pm |
| ring-runner-pm | 3 | 3 | 86400s | pm |
| sim-alpha | 5 | 10 | 43200s | test-dummy |
| sim-beta | 8 | 20 | 43200s | test-dummy |
| sim-delta | 9 | 25 | 43200s | test-dummy |
| sim-gamma | 3 | 15 | 43200s | test-dummy |
| SpecLang | 10 | 15 | 43200s | coding-hermes |
| speclang-pm | 3 | 3 | 86400s | pm |
| task-router | 10 | 10 | 21600s | coding-hermes |
| temple-runner-pm | 3 | 3 | 86400s | pm |
| terminal-jail-pm | 3 | 3 | 86400s | pm |
| totalstack-pm | 3 | 3 | 86400s | pm |
| uhlp-pm | 3 | 3 | 86400s | pm |
| warpfs-pm | 3 | 3 | 86400s | pm |
| wojons-mythos-pm | 3 | 3 | 86400s | pm |
| zz-gap12-probe | 5 | 10 | 900s | - |
| zz-schedgap-011-probe | 5 | 10 | 900s | - |

## Live Dashboard

Point a browser at http://127.0.0.1:9090/ for the live HTML dashboard (auto-refreshes; per-project detail, queue, tick history, health).
