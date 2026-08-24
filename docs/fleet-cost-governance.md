# Fleet Cost Governance — Design Spec

> Source: Bane directive 2026-08-23. Board rows: SCHED-GAP-064..068.
> Context: fleet 16,259 ticks / 9.48B input tokens / 73.5M output tokens / ~$2,017 est
> (Jul 12–Aug 23). Input:output ratio ≈ **129:1** — context re-reads dominate, not generation.

## 1. Cooldown policy (DONE 2026-08-23)

All five 1h projects moved to the 6h default (live API + fleet.toml pins + policy-script ok):

| Project | Before | After |
|---|---|---|
| 9router | 3600 | 21600 |
| escalation-doctrine | 3600 | 21600 |
| hermes-canopy | 3600 | 21600 |
| hermes-dagger | 3600 | 21600 |
| musterflow | 3600 | 21600 |

Policy: **most projects 4–6h** (21600 default). 3600 reserved for explicitly
Bane-designated fast projects. 43200 for completed/idle. `fleet-cooldown-policy.py`
enforces floors/promotions; operator pins live in fleet.toml.

## 2. Model/provider fallback chains (SCHED-GAP-064)

### Resolution order (per spawn)

```
1. project.primary (model+provider)          # e.g. custom key, deepseek-foreman
2. project.fallback (model+provider)         # if primary unavailable/fails
3. GLOBAL primary (model+provider)           # fleet-wide default
4. GLOBAL fallback (model+provider)          # last resort
```

- A per-project `no_global_fallback = true` flag stops the chain at step 2
  (project opted out of global lanes entirely — e.g. isolated custom-key projects).
- Failure detection: provider auth failure, 402/403 exhaustion, repeated errors —
  NOT a single transient 5xx (avoid flapping). Same classification as the existing
  gateway-key-rejected terminal path.
- Both spawn paths must resolve identically: gateway HTTP spawn (passes `provider`
  in the body — fix f3919a7) AND exec fallback (`--provider`).
- fleet.toml schema: `primary_model/primary_provider/fallback_model/fallback_provider`
  per project + `[global] primary/fallback` section. Empty fields = skip that step.

### Why
Different projects have different economics: some ride flat-rate subs
(opencode-go, ollama-cloud, kimi-for-coding), some PAYG lanes, some custom keys.
A broken custom provider currently strands the project (or silently falls to the
gateway default = main key — the f3919a7 bug class). Explicit chains fix both.

## 3. Idle-tick / NEVER-DONE model routing (SCHED-GAP-065 — DONE 2026-08-24)

- Per-project `idle_model`/`idle_provider` in fleet.toml (conditional pin,
  same contract as the SCHED-GAP-064 fallback tiers) + spawner env defaults
  `SCHEDULER_FOREMAN_IDLE_MODEL`/`_PROVIDER` as the global idle lane.
- Chain kind is decided PRE-SPAWN: the spawner counts the project's board
  pending rows via the existing PendingTaskCounter (SCHED-GAP-019 —
  tasks.jsonl `status=="pending"`, tasks.md `"## [ ] "` fallback; 0 when no
  board file). Zero pending = idle tick, else work tick. Counter semantics
  unchanged — fixture rows count as pending, so fixture-carrying projects
  keep the work chain.
- Idle chain = idle tiers PREPENDED to the regular SCHED-GAP-064 chain:
  `project idle → global idle (env) → project primary → project fallback →
  global primary → global fallback`. The env idle lane is gated by
  `no_global_fallback` like the other spawner-level tiers. Empty idle fields
  are skipped, so a project with no idle config resolves exactly as before
  (no regression).
- The gateway 401/403 retry (one-step chain advance, SCHED-GAP-064) walks
  the SAME chain the tick was spawned with — a rejected idle tick retries
  within the idle chain, never jumps straight to the work lane.
- Every dispatch logs `SPAWN: <project> tick=<id> chain=idle|work
  model="…" provider="…"` — the audit trail for which lane each tick paid.
- DB: migration v16 (`idle_model`/`idle_provider` TEXT NOT NULL DEFAULT '');
  API JSON fields + `PUT /api/v1/projects/{name}` accept the snake_case keys.
- Worker model text (WorkerDefaults) is the WORKER lane, not the foreman
  tick lane — deliberately untouched.

## 4. Per-project budgets (SCHED-GAP-066 — DONE 2026-08-24)

### Rules
- **Never kill a running tick.** Budgets gate *spawns only* (selection-time
  filter in all three packer paths; running tick rows are untouched).
- Tiers per project (all optional, any combination; `0`/unset = unlimited):
  - `daily_budget_usd` — resets at the UTC day boundary
  - `weekly_budget_usd` — resets Monday 00:00 UTC
  - `final_budget_usd` — lifetime cap; when exhausted the project stops
    scheduling for good (e.g. inference-estimator: one-time project, fixed
    budget, then done).
- Budget source of truth: scheduler.db `ticks.cost_usd` sums (already recorded
  per tick).
- Exhausted state: `/api/v1/projects` shows `blocked_reason = "budget"` +
  `budget_window = "daily"|"weekly"|"final"`, `budget_blocked = true`,
  `spent_daily/weekly/total_usd`, `remaining_*` (null when uncapped);
  zero new spawns, existing tick completes normally. The scheduler log line is
  `BUDGET: <project> blocked (<window> spent $X/$Y)`.
- Schema: migration v15 adds the three REAL columns (default 0.0); fleet.toml
  keys pin on restart when present (explicit `0` clears; keyless entries leave
  API-assigned caps untouched); `PUT /api/v1/projects/{name}` accepts the
  snake_case keys.

## 5. Skill bloat + fresh-context management (SCHED-GAP-067 — DONE 2026-08-24)

Full policy: `docs/skill-size-policy.md`. Audit + split results:
`docs/skill-bloat-report-2026-08-24.md` (19 fleet skills split from ~100K to
≤50K; fleet >50K count 97→78; zero fleet skills >100K remain).

- Fleet-wide SKILL.md size scan (cron or one-shot): >100K = emergency, >50K = trim.
  Move pitfalls corpora to `references/`; keep the body lean (proven: 100K→48.6K
  saved ~240M tokens/day).
- Fresh-context (cache-miss) discipline for tick authors:
  1. **Byte-stable context within a tick** — don't swap skills/system prompt
     mid-tick; every mutation invalidates the cache prefix and re-pays at miss
     prices (2–31× hit price).
  2. **Batch LLM calls** — cache TTL ~5 min; a call after a long terminal gap
     re-pays the whole context as a miss. Do reads/checks first, then LLM calls
     back-to-back.
  3. Keep tick prompts as short as correctness allows (the 15-skill load list
     is re-paid per call).

## 6. Weekly cost digest (SCHED-GAP-068)

- Script: `scheduler.db` → per-project ticks/tokens_in/tokens_out/cost_usd +
  in:out ratio + budget status; merge DeepSeek usage export when available.
- Deliver: Telegram table + DuckBrain finding, weekly. Numbers must match
  scheduler.db exactly (verification step in the cron prompt).

## 7. Supporting changes

- `fleet.toml` regen script must preserve all new fields (it currently rewrites
  only name/repo/weight/priority/cooldown/model/provider/ns/deliver/enabled —
  extend `write_fleet_pins`).
- Scheduler docs (docs/fleet.md, docs/api.md) updated for the new schema.
