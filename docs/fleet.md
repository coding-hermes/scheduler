# Coding Hermes Fleet — Live Status

**Generated 2026-09-18 05:02 UTC from the live schedulerd API** (`GET http://127.0.0.1:9090/api/v1/status` + `/api/v1/projects`). Do not edit by hand — run `python3 docs/regenerate_fleet.py` to refresh.

## Settings (live)

| Setting | Value |
|---------|-------|
| Active projects (enabled) | 69 |
| Total projects (incl. disabled) | 261 |
| Active ticks | 10 |
| Budget | 100 |
| Last evaluation | 2026-09-18T04:59:20Z |
| Recent outcomes | completed=27678, failed=40064, timeout=4152 |
| DuckBrain sync | reachable=True, spooled_pending=0 |

## Fleet (261 projects, 69 enabled)

### Enabled (69)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| bunker | 10 | 15 | 21600s | coding-hermes |
| coding-hermes-scheduler | 10 | 15 | 21600s | coding-hermes |
| crier | 10 | 15 | 21600s | coding-hermes |
| duckbrain | 10 | 10 | 21600s | coding-hermes |
| hermes-canopy | 10 | 10 | 21600s | coding-hermes |
| hermes-dagger | 10 | 10 | 21600s | coding-hermes |
| task-router | 10 | 10 | 21600s | coding-hermes |
| terminal-jail | 10 | 15 | 21600s | coding-hermes |
| trouble | 10 | 10 | 21600s | coding-hermes |
| warpfs | 10 | 15 | 21600s | coding-hermes |
| gitreins-poc | 8 | 25 | 21600s | coding-hermes |
| coding-hermes-tools | 6 | 3 | 21600s | coding-hermes |
| 9router | 5 | 10 | 21600s | coding-hermes |
| 9router-dogfood | 5 | 3 | 259200s | dogfood |
| axiom-sync | 5 | 1 | 21600s | duckbrain-sync |
| blog-sync | 5 | 1 | 21600s | duckbrain-sync |
| boardctl | 5 | 3 | 21600s | coding-hermes |
| boardctl-sync | 5 | 1 | 21600s | duckbrain-sync |
| bunker-dogfood | 5 | 3 | 259200s | dogfood |
| bunker-sync | 5 | 1 | 21600s | duckbrain-sync |
| chimera-v2 | 5 | 10 | 21600s | coding-hermes |
| chimera-v2-dogfood | 5 | 3 | 259200s | dogfood |
| chimera-v2-sync | 5 | 1 | 21600s | duckbrain-sync |
| coding-hermes-sync | 5 | 1 | 21600s | duckbrain-sync |
| crier-dogfood | 5 | 3 | 86400s | dogfood |
| crier-sync | 5 | 1 | 21600s | duckbrain-sync |
| deepseek-payg-sync | 5 | 1 | 21600s | duckbrain-sync |
| duckbrain-sync | 5 | 1 | 21600s | duckbrain-sync |
| eduos-sync | 5 | 1 | 21600s | duckbrain-sync |
| frontiers-ghost-sync | 5 | 1 | 21600s | duckbrain-sync |
| gitreins-poc-dogfood | 5 | 3 | 259200s | dogfood |
| gitreins-sync | 5 | 1 | 21600s | duckbrain-sync |
| heading | 5 | 10 | 21600s | coding-hermes |
| heading-dogfood | 5 | 3 | 259200s | dogfood |
| heading-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-agent-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-canopy-dogfood | 5 | 3 | 259200s | dogfood |
| hermes-canopy-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-dagger-dogfood | 5 | 3 | 259200s | dogfood |
| my-project | 5 | 10 | 21600s | coding-hermes |
| off-by-one | 5 | 10 | 21600s | coding-hermes |
| off-by-one-dogfood | 5 | 3 | 259200s | dogfood |
| off-by-one-sync | 5 | 1 | 21600s | duckbrain-sync |
| reports-sync | 5 | 1 | 21600s | duckbrain-sync |
| task-router-sync | 5 | 1 | 21600s | duckbrain-sync |
| temporal-vector-index-sync | 5 | 1 | 21600s | duckbrain-sync |
| terminal-jail-dogfood | 5 | 3 | 259200s | dogfood |
| terminal-jail-sync | 5 | 1 | 21600s | duckbrain-sync |
| trouble-sync | 5 | 1 | 21600s | duckbrain-sync |
| warpfs-dogfood | 5 | 3 | 259200s | dogfood |
| warpfs-sync | 5 | 1 | 21600s | duckbrain-sync |
| 9router-qa | 4 | 4 | 21600s | qa |
| bunker-qa | 4 | 4 | 21600s | qa |
| chimera-v2-qa | 4 | 4 | 21600s | qa |
| crier-qa | 4 | 4 | 21600s | qa |
| gitreins-poc-qa | 4 | 4 | 21600s | qa |
| heading-qa | 4 | 4 | 21600s | qa |
| hermes-canopy-qa | 4 | 4 | 21600s | qa |
| hermes-dagger-qa | 4 | 4 | 21600s | qa |
| off-by-one-qa | 4 | 4 | 21600s | qa |
| qa-audit | 4 | 5 | 86400s | qa |
| terminal-jail-qa | 4 | 4 | 21600s | qa |
| trouble-dogfood | 4 | 3 | 259200s | dogfood |
| trouble-qa | 4 | 4 | 21600s | qa |
| warpfs-qa | 4 | 4 | 21600s | qa |
| coding-hermes-scheduler-pm | 3 | 3 | 21600s | pm |
| duckbrain-pm | 3 | 3 | 86400s | pm |
| my-project-pm | 3 | 3 | 86400s | pm |
| release-engineer | 3 | 5 | 604800s | releases |

### Disabled (192)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| 9router-pm | 3 | 3 | 86400s | pm |
| 9router-sync | 5 | 1 | 21600s | duckbrain-sync |
| ai-plays-poke | 10 | 15 | 21600s | coding-hermes |
| ai-plays-poke-dogfood | 5 | 3 | 259200s | dogfood |
| ai-plays-poke-pm | 3 | 3 | 86400s | pm |
| ai-plays-poke-qa | 4 | 4 | 43200s | qa |
| ai-plays-poke-sync | 5 | 1 | 21600s | duckbrain-sync |
| asce | 10 | 15 | 21600s | coding-hermes |
| asce-dogfood | 5 | 3 | 259200s | dogfood |
| asce-pm | 3 | 3 | 86400s | pm |
| asce-qa | 4 | 4 | 43200s | qa |
| asce-sync | 5 | 1 | 21600s | duckbrain-sync |
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
| consensus | 10 | 15 | 21600s | coding-hermes |
| consensus-dogfood | 5 | 3 | 259200s | dogfood |
| consensus-pm | 3 | 3 | 86400s | pm |
| consensus-qa | 4 | 4 | 21600s | qa |
| consensus-sync | 5 | 1 | 21600s | duckbrain-sync |
| crier-pm | 3 | 3 | 86400s | pm |
| dc-prune | 7 | 8 | 43200s | test-dummy |
| dc-rotate | 3 | 3 | 43200s | test-dummy |
| dc-vacuum | 5 | 5 | 43200s | test-dummy |
| deepseek-dashboard | 10 | 15 | 21600s | coding-hermes |
| deepseek-dashboard-dogfood | 5 | 3 | 259200s | dogfood |
| deepseek-dashboard-pm | 3 | 3 | 86400s | pm |
| deepseek-dashboard-qa | 4 | 4 | 43200s | qa |
| deepseek-dashboard-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-core | 5 | 10 | 21600s | coding-hermes |
| dexdat-core-dogfood | 5 | 3 | 259200s | dogfood |
| dexdat-core-pm | 3 | 3 | 86400s | pm |
| dexdat-core-qa | 4 | 4 | 43200s | qa |
| dexdat-core-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-memory | 10 | 10 | 21600s | coding-hermes |
| dexdat-memory-dogfood | 5 | 3 | 259200s | dogfood |
| dexdat-memory-pm | 3 | 3 | 86400s | pm |
| dexdat-memory-qa | 4 | 4 | 43200s | qa |
| dexdat-memory-sync | 5 | 1 | 21600s | duckbrain-sync |
| doc-writer | 2 | 3 | 604800s | doc-writer |
| dogfood-20260815 | 1 | 3 | 900s | - |
| dogfood-20260815-dup | 5 | 10 | 900s | - |
| dogfood-20260815-guard | 5 | 10 | 900s | - |
| eduos.dexdat.com.co | 5 | 10 | 21600s | coding-hermes |
| eduos.dexdat.com.co-dogfood | 5 | 3 | 259200s | dogfood |
| eduos.dexdat.com.co-pm | 3 | 3 | 86400s | pm |
| eduos.dexdat.com.co-qa | 4 | 4 | 43200s | qa |
| escalation-doctrine | 5 | 10 | 21600s | coding-hermes |
| escalation-doctrine-dogfood | 5 | 3 | 259200s | dogfood |
| escalation-doctrine-pm | 3 | 3 | 86400s | pm |
| escalation-doctrine-qa | 4 | 4 | 43200s | qa |
| escalation-doctrine-sync | 5 | 1 | 21600s | duckbrain-sync |
| gitreins-poc-pm | 3 | 3 | 86400s | pm |
| global-fast | 10 | 15 | 43200s | test-dummy |
| global-slow | 1 | 10 | 43200s | test-dummy |
| h3 | 10 | 15 | 43200s | coding-hermes |
| h3-dogfood | 5 | 3 | 259200s | dogfood |
| h3-pm | 3 | 3 | 86400s | pm |
| h3-qa | 4 | 4 | 43200s | qa |
| h3-sdk-go-foreman | 8 | 10 | 43200s | coding-hermes |
| h3-sdk-go-foreman-dogfood | 5 | 3 | 259200s | dogfood |
| h3-sdk-go-foreman-pm | 3 | 3 | 86400s | pm |
| h3-sdk-go-foreman-qa | 4 | 4 | 43200s | qa |
| h3-sdk-python-foreman | 10 | 15 | 43200s | coding-hermes |
| h3-sdk-python-foreman-dogfood | 5 | 3 | 259200s | dogfood |
| h3-sdk-python-foreman-pm | 3 | 3 | 86400s | pm |
| h3-sdk-python-foreman-qa | 4 | 4 | 43200s | qa |
| h3-sdk-typescript-foreman | 5 | 10 | 43200s | coding-hermes |
| h3-sdk-typescript-foreman-dogfood | 5 | 3 | 259200s | dogfood |
| h3-sdk-typescript-foreman-pm | 3 | 3 | 86400s | pm |
| h3-sdk-typescript-foreman-qa | 4 | 4 | 43200s | qa |
| h3-shim-foreman | 10 | 15 | 43200s | coding-hermes |
| h3-shim-foreman-dogfood | 5 | 3 | 259200s | dogfood |
| h3-shim-foreman-pm | 3 | 3 | 86400s | pm |
| h3-shim-foreman-qa | 4 | 4 | 43200s | qa |
| h3-umbrella-sync | 5 | 1 | 21600s | duckbrain-sync |
| HEADING | 10 | 25 | 43200s | coding-hermes |
| heading-pm | 3 | 3 | 86400s | pm |
| helios | 8 | 10 | 21600s | coding-hermes |
| helios-dogfood | 5 | 3 | 259200s | dogfood |
| helios-pm | 3 | 3 | 86400s | pm |
| helios-qa | 4 | 4 | 43200s | qa |
| helios-sync | 5 | 1 | 21600s | duckbrain-sync |
| helix | 10 | 10 | 21600s | coding-hermes |
| helix-dogfood | 5 | 3 | 259200s | dogfood |
| helix-pm | 3 | 3 | 86400s | pm |
| helix-qa | 4 | 4 | 43200s | qa |
| helix-sync | 5 | 1 | 21600s | duckbrain-sync |
| hermes-canopy-pm | 3 | 3 | 86400s | pm |
| hermes-dagger-pm | 3 | 3 | 86400s | pm |
| hermes4friends-infra | 5 | 10 | 21600s | coding-hermes |
| hermes4friends-infra-dogfood | 5 | 3 | 259200s | dogfood |
| hermes4friends-infra-pm | 3 | 3 | 86400s | pm |
| hermes4friends-infra-qa | 4 | 4 | 43200s | qa |
| hermes4friends-infra-sync | 5 | 1 | 21600s | duckbrain-sync |
| hivemind-pulse | 10 | 15 | 43200s | coding-hermes |
| hivemind-sync | 5 | 1 | 21600s | duckbrain-sync |
| hivemind-work | 10 | 15 | 21600s | coding-hermes |
| hivemind-work-dogfood | 5 | 3 | 259200s | dogfood |
| hivemind-work-pm | 3 | 3 | 86400s | pm |
| hivemind-work-qa | 4 | 4 | 21600s | qa |
| imhotep | 5 | 10 | 21600s | coding-hermes |
| imhotep-dogfood | 5 | 3 | 259200s | dogfood |
| imhotep-pm | 3 | 3 | 86400s | pm |
| imhotep-qa | 4 | 4 | 43200s | qa |
| inference-estimator | 8 | 10 | 21600s | coding-hermes |
| inference-estimator-dogfood | 5 | 3 | 259200s | dogfood |
| inference-estimator-pm | 3 | 3 | 86400s | pm |
| inference-estimator-qa | 4 | 4 | 43200s | qa |
| inference-estimator-sync | 5 | 1 | 21600s | duckbrain-sync |
| Kobayashi-Maru | 10 | 15 | 21600s | coding-hermes |
| Kobayashi-Maru-dogfood | 5 | 3 | 259200s | dogfood |
| Kobayashi-Maru-pm | 3 | 3 | 86400s | pm |
| Kobayashi-Maru-qa | 4 | 4 | 43200s | qa |
| kobayashi-maru-sync | 5 | 1 | 21600s | duckbrain-sync |
| mafia-ai-benchmark | 8 | 10 | 21600s | coding-hermes |
| mafia-ai-benchmark-dogfood | 5 | 3 | 259200s | dogfood |
| mafia-ai-benchmark-pm | 3 | 3 | 86400s | pm |
| mafia-ai-benchmark-qa | 4 | 4 | 43200s | qa |
| mafia-benchmark-sync | 5 | 1 | 21600s | duckbrain-sync |
| mon-alert | 4 | 5 | 43200s | test-dummy |
| mon-check | 6 | 5 | 43200s | test-dummy |
| mon-ping | 8 | 10 | 43200s | test-dummy |
| muster | 10 | 15 | 604800s | coding-hermes |
| muster-dogfood | 5 | 3 | 604800s | dogfood |
| muster-pm | 3 | 3 | 86400s | pm |
| muster-qa | 4 | 4 | 604800s | qa |
| muster-sync | 5 | 1 | 21600s | duckbrain-sync |
| musterflow | 10 | 15 | 21600s | coding-hermes |
| musterflow-dogfood | 5 | 3 | 259200s | dogfood |
| musterflow-pm | 3 | 3 | 86400s | pm |
| musterflow-qa | 4 | 4 | 43200s | qa |
| musterflow-sync | 5 | 1 | 21600s | duckbrain-sync |
| mythos | 5 | 10 | 900s | coding-hermes |
| mythos-sync | 5 | 1 | 21600s | duckbrain-sync |
| off-by-one-pm | 3 | 3 | 86400s | pm |
| rabbit-hole | 10 | 15 | 21600s | coding-hermes |
| rabbit-hole-dogfood | 5 | 3 | 259200s | dogfood |
| rabbit-hole-pm | 3 | 3 | 86400s | pm |
| rabbit-hole-qa | 4 | 4 | 43200s | qa |
| rabbit-hole-sync | 5 | 1 | 21600s | duckbrain-sync |
| rethinkdb | 5 | 10 | 21600s | coding-hermes |
| rethinkdb-dogfood | 5 | 3 | 259200s | dogfood |
| rethinkdb-pm | 3 | 3 | 86400s | pm |
| rethinkdb-qa | 4 | 4 | 43200s | qa |
| rethinkdb-sync | 5 | 1 | 21600s | duckbrain-sync |
| ring-runner | 10 | 15 | 21600s | coding-hermes |
| ring-runner-dogfood | 5 | 3 | 259200s | dogfood |
| ring-runner-pm | 3 | 3 | 86400s | pm |
| ring-runner-qa | 4 | 4 | 43200s | qa |
| ring-runner-sync | 5 | 1 | 21600s | duckbrain-sync |
| sim-alpha | 5 | 10 | 43200s | test-dummy |
| sim-beta | 8 | 20 | 43200s | test-dummy |
| sim-delta | 9 | 25 | 43200s | test-dummy |
| sim-gamma | 3 | 15 | 43200s | test-dummy |
| SpecLang | 10 | 15 | 43200s | coding-hermes |
| speclang | 10 | 15 | 21600s | coding-hermes |
| speclang-dogfood | 5 | 3 | 259200s | dogfood |
| speclang-pm | 3 | 3 | 86400s | pm |
| speclang-qa | 4 | 4 | 43200s | qa |
| speclang-sync | 5 | 1 | 21600s | duckbrain-sync |
| temple-runner | 10 | 10 | 604800s | coding-hermes |
| temple-runner-dogfood | 5 | 3 | 604800s | dogfood |
| temple-runner-pm | 3 | 3 | 86400s | pm |
| temple-runner-qa | 4 | 4 | 604800s | qa |
| temple-runner-sync | 5 | 1 | 21600s | duckbrain-sync |
| terminal-jail-pm | 3 | 3 | 86400s | pm |
| totalstack | 10 | 15 | 21600s | coding-hermes |
| totalstack-dogfood | 5 | 3 | 259200s | dogfood |
| totalstack-pm | 3 | 3 | 86400s | pm |
| totalstack-qa | 4 | 4 | 43200s | qa |
| totalstack-sync | 5 | 1 | 21600s | duckbrain-sync |
| uhlp | 9 | 15 | 21600s | coding-hermes |
| uhlp-dogfood | 5 | 3 | 259200s | dogfood |
| uhlp-pm | 3 | 3 | 86400s | pm |
| uhlp-qa | 4 | 4 | 43200s | qa |
| uhlp-sync | 5 | 1 | 21600s | duckbrain-sync |
| warpfs-pm | 3 | 3 | 86400s | pm |
| wojons-mythos | 10 | 15 | 21600s | coding-hermes |
| wojons-mythos-dogfood | 5 | 3 | 259200s | dogfood |
| wojons-mythos-pm | 3 | 3 | 86400s | pm |
| wojons-mythos-qa | 4 | 4 | 43200s | qa |
| zz-gap12-probe | 5 | 10 | 900s | - |
| zz-schedgap-011-probe | 5 | 10 | 900s | - |

## Live Dashboard

Point a browser at http://127.0.0.1:9090/ for the live HTML dashboard (auto-refreshes; per-project detail, queue, tick history, health).
