# Coding Hermes Fleet — Live Status

**Generated 2026-10-14 07:53 UTC from the live schedulerd API** (`GET http://127.0.0.1:9090/api/v1/status` + `/api/v1/projects`). Do not edit by hand — run `python3 docs/regenerate_fleet.py` to refresh.

## Settings (live)

| Setting | Value |
|---------|-------|
| Active projects (enabled) | 400 |
| Total projects (incl. disabled) | 200 |
| Active ticks | 10 |
| Budget | 100 |
| Last evaluation | 2026-10-14T07:48:06Z |
| Recent outcomes | completed=41570, failed=42400, timeout=4278 |
| DuckBrain sync | reachable=True, spooled_pending=0 |

## Fleet (200 projects, 151 enabled)

### Enabled (151)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| ai-plays-poke-foreman | 10 | 15 | 21600s | coding-hermes |
| asce-foreman | 10 | 15 | 21600s | coding-hermes |
| bunker-foreman | 10 | 15 | 21600s | coding-hermes |
| coding-hermes-scheduler-foreman | 10 | 15 | 21600s | coding-hermes |
| consensus-foreman | 10 | 15 | 21600s | coding-hermes |
| crier-foreman | 10 | 15 | 21600s | coding-hermes |
| dexdat-memory-foreman | 10 | 10 | 21600s | coding-hermes |
| duckbrain-foreman | 10 | 10 | 21600s | coding-hermes |
| coding-hermes-tools-foreman | 6 | 3 | 3600s | coding-hermes |
| 9router-docs | 5 | 3 | 604800s | docs |
| 9router-dogfood | 5 | 3 | 259200s | dogfood |
| 9router-foreman | 5 | 10 | 21600s | coding-hermes |
| 9router-readme | 5 | 3 | 604800s | docs |
| 9router-sync | 5 | 1 | 21600s | duckbrain-sync |
| ai-plays-poke-docs | 5 | 3 | 604800s | docs |
| ai-plays-poke-dogfood | 5 | 3 | 259200s | dogfood |
| ai-plays-poke-perf | 5 | 3 | 21600s | perf |
| ai-plays-poke-readme | 5 | 3 | 604800s | docs |
| ai-plays-poke-sync | 5 | 1 | 21600s | duckbrain-sync |
| asce-docs | 5 | 3 | 604800s | docs |
| asce-dogfood | 5 | 3 | 259200s | dogfood |
| asce-readme | 5 | 3 | 604800s | docs |
| asce-sync | 5 | 1 | 21600s | duckbrain-sync |
| auger-docs | 5 | 3 | 604800s | docs |
| auger-dogfood | 5 | 3 | 259200s | dogfood |
| auger-foreman | 5 | 10 | 21600s | coding-hermes |
| auger-readme | 5 | 3 | 604800s | docs |
| auger-sync | 5 | 1 | 21600s | duckbrain-sync |
| axiom-sync | 5 | 1 | 21600s | duckbrain-sync |
| blog-sync | 5 | 1 | 21600s | duckbrain-sync |
| boardctl-docs | 5 | 3 | 604800s | docs |
| boardctl-foreman | 5 | 3 | 21600s | coding-hermes |
| boardctl-readme | 5 | 3 | 604800s | docs |
| boardctl-sync | 5 | 1 | 21600s | duckbrain-sync |
| bunker-docs | 5 | 3 | 604800s | docs |
| bunker-dogfood | 5 | 3 | 259200s | dogfood |
| bunker-perf | 5 | 3 | 604800s | perf |
| bunker-readme | 5 | 3 | 604800s | docs |
| bunker-sync | 5 | 1 | 21600s | duckbrain-sync |
| chimera-v2-docs | 5 | 3 | 604800s | docs |
| chimera-v2-dogfood | 5 | 3 | 259200s | dogfood |
| chimera-v2-foreman | 5 | 10 | 21600s | coding-hermes |
| chimera-v2-readme | 5 | 3 | 604800s | docs |
| chimera-v2-sync | 5 | 1 | 21600s | duckbrain-sync |
| coding-hermes-scheduler-docs | 5 | 3 | 604800s | docs |
| coding-hermes-scheduler-perf | 5 | 3 | 604800s | perf |
| coding-hermes-scheduler-readme | 5 | 3 | 604800s | docs |
| coding-hermes-skills-docs | 5 | 3 | 604800s | docs |
| coding-hermes-sync | 5 | 1 | 21600s | duckbrain-sync |
| coding-hermes-tools-docs | 5 | 3 | 604800s | docs |
| coding-hermes-tools-readme | 5 | 3 | 604800s | docs |
| consensus-docs | 5 | 3 | 604800s | docs |
| consensus-dogfood | 5 | 3 | 259200s | dogfood |
| consensus-readme | 5 | 3 | 604800s | docs |
| consensus-sync | 5 | 1 | 21600s | duckbrain-sync |
| crier-docs | 5 | 3 | 604800s | docs |
| crier-dogfood | 5 | 3 | 259200s | dogfood |
| crier-readme | 5 | 3 | 604800s | docs |
| crier-sync | 5 | 1 | 21600s | duckbrain-sync |
| deepseek-dashboard-docs | 5 | 3 | 604800s | docs |
| deepseek-dashboard-readme | 5 | 3 | 604800s | docs |
| deepseek-payg-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-core-docs | 5 | 3 | 604800s | docs |
| dexdat-core-dogfood | 5 | 3 | 259200s | dogfood |
| dexdat-core-foreman | 5 | 10 | 21600s | coding-hermes |
| dexdat-core-readme | 5 | 3 | 604800s | docs |
| dexdat-core-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-memory-docs | 5 | 3 | 604800s | docs |
| dexdat-memory-dogfood | 5 | 3 | 259200s | dogfood |
| dexdat-memory-readme | 5 | 3 | 604800s | docs |
| dexdat-memory-sync | 5 | 1 | 21600s | duckbrain-sync |
| digest-docs | 5 | 3 | 604800s | docs |
| digest-dogfood | 5 | 3 | 259200s | dogfood |
| digest-foreman | 5 | 10 | 21600s | coding-hermes |
| digest-readme | 5 | 3 | 604800s | docs |
| digest-sync | 5 | 1 | 21600s | duckbrain-sync |
| duckbrain-docs | 5 | 3 | 604800s | docs |
| duckbrain-perf | 5 | 3 | 604800s | perf |
| duckbrain-readme | 5 | 3 | 604800s | docs |
| duckbrain-sync | 5 | 1 | 21600s | duckbrain-sync |
| eduos-sync | 5 | 1 | 21600s | duckbrain-sync |
| escalation-doctrine-docs | 5 | 3 | 604800s | docs |
| Kobayashi-Maru-docs | 5 | 3 | 604800s | docs |
| Kobayashi-Maru-readme | 5 | 3 | 604800s | docs |
| 9router-qa | 4 | 4 | 21600s | qa |
| 9router-review | 4 | 3 | 604800s | review |
| ai-plays-poke-qa | 4 | 4 | 21600s | qa |
| ai-plays-poke-releng | 4 | 4 | 21600s | releases |
| ai-plays-poke-review | 4 | 3 | 604800s | review |
| asce-qa | 4 | 4 | 21600s | qa |
| asce-review | 4 | 3 | 604800s | review |
| auger-pm | 4 | 4 | 21600s | pm |
| auger-qa | 4 | 4 | 21600s | qa |
| auger-releng | 4 | 4 | 21600s | releases |
| auger-review | 4 | 3 | 604800s | review |
| boardctl-dogfood | 4 | 4 | 259200s | dogfood |
| boardctl-pm | 4 | 4 | 21600s | pm |
| boardctl-qa | 4 | 4 | 21600s | qa |
| boardctl-review | 4 | 3 | 604800s | review |
| bunker-qa | 4 | 4 | 21600s | qa |
| bunker-review | 4 | 3 | 604800s | review |
| chimera-v2-qa | 4 | 4 | 21600s | qa |
| chimera-v2-review | 4 | 3 | 604800s | review |
| coding-hermes-scheduler-dogfood | 4 | 4 | 259200s | dogfood |
| coding-hermes-scheduler-qa | 4 | 4 | 21600s | qa |
| coding-hermes-scheduler-review | 4 | 3 | 604800s | review |
| coding-hermes-scheduler-sync | 4 | 4 | 21600s | duckbrain-sync |
| coding-hermes-tools-dogfood | 4 | 4 | 259200s | dogfood |
| coding-hermes-tools-pm | 4 | 4 | 21600s | pm |
| coding-hermes-tools-qa | 4 | 4 | 21600s | qa |
| coding-hermes-tools-review | 4 | 3 | 604800s | review |
| coding-hermes-tools-sync | 4 | 4 | 21600s | duckbrain-sync |
| consensus-qa | 4 | 4 | 21600s | qa |
| consensus-releng | 4 | 4 | 86400s | releases |
| consensus-review | 4 | 3 | 604800s | review |
| crier-qa | 4 | 4 | 21600s | qa |
| crier-review | 4 | 3 | 604800s | review |
| dexdat-core-qa | 4 | 4 | 21600s | qa |
| dexdat-core-releng | 4 | 4 | 21600s | releases |
| dexdat-core-review | 4 | 3 | 604800s | review |
| dexdat-memory-qa | 4 | 4 | 21600s | qa |
| dexdat-memory-releng | 4 | 4 | 21600s | releases |
| dexdat-memory-review | 4 | 3 | 604800s | review |
| digest-perf | 4 | 3 | 604800s | perf |
| digest-pm | 4 | 4 | 21600s | pm |
| digest-qa | 4 | 4 | 21600s | qa |
| digest-releng | 4 | 4 | 21600s | releases |
| digest-review | 4 | 3 | 604800s | review |
| duckbrain-dogfood | 4 | 4 | 259200s | dogfood |
| duckbrain-qa | 4 | 4 | 21600s | qa |
| duckbrain-review | 4 | 3 | 604800s | review |
| 9router-pm | 3 | 3 | 21600s | pm |
| 9router-releng | 3 | 2 | 21600s | releases |
| ai-plays-poke-pm | 3 | 3 | 86400s | pm |
| asce-pm | 3 | 3 | 86400s | pm |
| asce-releng | 3 | 2 | 86400s | releases |
| boardctl-releng | 3 | 2 | 21600s | releases |
| bunker-pm | 3 | 3 | 21600s | pm |
| bunker-releng | 3 | 2 | 21600s | releases |
| chimera-v2-pm | 3 | 3 | 21600s | pm |
| chimera-v2-releng | 3 | 2 | 21600s | releases |
| coding-hermes-scheduler-pm | 3 | 3 | 21600s | pm |
| coding-hermes-scheduler-releng | 3 | 2 | 21600s | releases |
| coding-hermes-tools-releng | 3 | 2 | 21600s | releases |
| consensus-pm | 3 | 3 | 86400s | pm |
| crier-pm | 3 | 3 | 21600s | pm |
| crier-releng | 3 | 2 | 21600s | releases |
| dexdat-core-pm | 3 | 3 | 21600s | pm |
| dexdat-memory-pm | 3 | 3 | 21600s | pm |
| duckbrain-pm | 3 | 3 | 21600s | pm |
| duckbrain-releng | 3 | 2 | 21600s | releases |

### Disabled (49)

| Project | Priority | Weight | Cooldown | Namespace |
|---------|----------|--------|----------|-----------|
| 9router-perf | 5 | 3 | 604800s | perf |
| asce-perf | 5 | 3 | 604800s | perf |
| boardctl-perf | 5 | 3 | 604800s | perf |
| ch-alpha | 9 | 35 | 43200s | coding-hermes |
| ch-beta | 8 | 25 | 43200s | coding-hermes |
| ch-delta | 6 | 5 | 43200s | coding-hermes |
| ch-epsilon | 5 | 5 | 43200s | coding-hermes |
| ch-eta | 2 | 5 | 43200s | coding-hermes |
| ch-gamma | 7 | 10 | 43200s | coding-hermes |
| ch-zeta | 4 | 5 | 43200s | coding-hermes |
| chimera-v2-perf | 5 | 3 | 604800s | perf |
| coding-hermes-tools-perf | 5 | 3 | 604800s | perf |
| consensus-perf | 5 | 3 | 604800s | perf |
| crier-perf | 5 | 3 | 604800s | perf |
| dc-prune | 7 | 8 | 43200s | coding-hermes |
| dc-rotate | 3 | 3 | 43200s | coding-hermes |
| dc-vacuum | 5 | 5 | 43200s | coding-hermes |
| deepseek-dashboard-dogfood | 5 | 3 | 259200s | dogfood |
| deepseek-dashboard-foreman | 10 | 15 | 21600s | coding-hermes |
| deepseek-dashboard-perf | 5 | 3 | 604800s | perf |
| deepseek-dashboard-pm | 3 | 3 | 86400s | pm |
| deepseek-dashboard-qa | 4 | 4 | 21600s | qa |
| deepseek-dashboard-sync | 5 | 1 | 21600s | duckbrain-sync |
| dexdat-core-perf | 5 | 3 | 604800s | perf |
| dexdat-memory-perf | 5 | 3 | 604800s | perf |
| doc-writer-foreman | 2 | 3 | 604800s | doc-writer |
| dogfood-20260815 | 1 | 3 | 900s | coding-hermes |
| dogfood-20260815-dup | 5 | 10 | 900s | coding-hermes |
| dogfood-20260815-guard | 5 | 10 | 900s | coding-hermes |
| dogfood-probe-0925 | 5 | 10 | 1800s | coding-hermes |
| eduos.dexdat.com.co-docs | 5 | 3 | 604800s | docs |
| eduos.dexdat.com.co-dogfood | 5 | 3 | 259200s | dogfood |
| eduos.dexdat.com.co-foreman | 5 | 10 | 21600s | coding-hermes |
| eduos.dexdat.com.co-perf | 5 | 3 | 604800s | perf |
| eduos.dexdat.com.co-pm | 3 | 3 | 86400s | pm |
| eduos.dexdat.com.co-qa | 4 | 4 | 21600s | qa |
| eduos.dexdat.com.co-readme | 5 | 3 | 604800s | docs |
| escalation-doctrine-dogfood | 5 | 3 | 259200s | dogfood |
| escalation-doctrine-foreman | 5 | 10 | 21600s | coding-hermes |
| escalation-doctrine-perf | 5 | 3 | 604800s | perf |
| escalation-doctrine-pm | 3 | 3 | 86400s | pm |
| escalation-doctrine-qa | 4 | 4 | 21600s | qa |
| HEADING-foreman | 10 | 25 | 43200s | coding-hermes |
| Kobayashi-Maru-dogfood | 5 | 3 | 259200s | dogfood |
| Kobayashi-Maru-foreman | 10 | 15 | 21600s | coding-hermes |
| Kobayashi-Maru-perf | 5 | 3 | 604800s | perf |
| Kobayashi-Maru-pm | 3 | 3 | 86400s | pm |
| Kobayashi-Maru-qa | 4 | 4 | 21600s | qa |
| SpecLang-foreman | 10 | 15 | 43200s | coding-hermes |

## Live Dashboard

Point a browser at http://127.0.0.1:9090/ for the live HTML dashboard (auto-refreshes; per-project detail, queue, tick history, health).
