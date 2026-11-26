# SCHED-GAP-16953 — task-router redeploy proof (2026-11-26)

Tick: coding-hermes-scheduler-foreman-2026-11-26-14-27-34
Row: SCHED-GAP-16953 (P1) "the live router serves a build loaded 2026-10-19: TR-354 and the new coverage block are not live"

## Premise evolution (measured before dispatch)

- Original premise (2026-10-29): router served build 187bcae loaded 2026-10-19.
- Measured this tick, pre-redeploy: /health reported loaded_commit=216a7bd831ac,
  loaded_at=2026-11-23T19:05:14+00:00 (a sibling/ops tick had already reloaded 11-23),
  repo_commit=78a6b14c0c6a, stale=true. TR-354's fix commit 080f695e
  ("fix(routing): outcome-floor gate reads the lane's TOTAL record; verdict never
  shadowed (TR-354 follow-up)") IS an ancestor of loaded 216a7bd8 (verified via
  `git merge-base --is-ancestor 080f695e19a0 216a7bd831ac`), so the stale-premise
  part was dead — but AC1 demands loaded==deployed main with stale=false, which the
  3-day-old build did not satisfy. Row therefore required a REAL redeploy, not an
  evidence-only close.

## Redeploy (foreman-direct; zero code changes — repo tip 78a6b14c already on disk)

- `systemctl --user restart task-router-server.service` → active; loaded_at
  2026-11-26T14:31:39+00:00.
- `systemctl --user restart task-router-proxy.service` → active (serving arm same
  codebase; journal shows stop took ~61s draining in-flight work).

## AC verification (live process, not file mtime)

1. AC1 PASS — /health after restart: status=ok, commit=loaded=repo=78a6b14c0c6a,
   stale=false (both top-level and .code.stale), loaded_at 2026-11-26T14:31:39Z.
2. AC2 PASS — /health.coverage served: basis=price-first, coverage 0.0388 < floor
   0.5, reason "price-first: 44 of 1133 measured (coverage 0.039 < floor 0.5)",
   window_h 24, source outcomes-averages.jsonl 5735 rows, error=null.
   /model_status serves 2264 lanes, each carrying a measurement object
   (basis/measured/n_samples/success_rate/window_h/floor_samples — verified on a
   sampled lane, aws-bedrock/amazon.nova-2-lite-v1:0).
3. AC3 PASS — `python3 scripts/proxy_e2e.py --session schedgap16953-restart-<epoch>
   --prompt "Reply with the single word: pong"` AFTER both restarts: proxy_e2e —
   PASS; outcome=served; hops attempted 1; served hop 334 (xkiro/openai/gpt-6-luna);
   ledger row exists for the session; cost $0.0048309 public split; row
   interpretable ("row[0] served-with-chain"); flow artefacts found (envelope,
   ledger_row). A chain-only GET /resolve?project=coding-hermes-scheduler on :9391
   also returned 200 with the full chain (head hop 15 xkiro/open…).
4. AC4 recorded — pre/post loaded commits: pre 216a7bd831ac (loaded
   2026-11-23T19:05:14Z) → post 78a6b14c0c6a (loaded 2026-11-26T14:31:39Z).
   Rollback not needed: fleet resolve AND serve proven post-restart (AC3).
   SCHED-GAP-16938's proxy drift arm is NOT yet landed, so its gating remains
   conditional exactly as the row text allows ("once it lands").

## Residual

- SCHED-GAP-16938 (drift INSTRUMENT) remains pending — with it landed, a stale
  build cannot silently serve again. This row's premise class (stale serving build)
  is now closed by the redeploy itself; recurrence protection is owned by -16938.
