# Usage-pool operations (SCHED-GAP-1726)

Usage pools are opt-in and are owned by the scheduler's configured SQLite database. They are not bunker-local and do not coordinate independent schedulers with separate databases. Leave `[usage_pools].enabled = false` (the default) to preserve existing admission behavior.

Example root `schedulerd.toml` configuration:

```toml
[usage_pools]
enabled = true
observe_only = true
local_pool_id = "local:control"

[[usage_pools.pools]]
id = "local:control"
kind = "local"
active_limit = 12

[[usage_pools.pools]]
id = "host:build-01"
kind = "host"
active_limit = 8

[[usage_pools.pools]]
id = "project:helix"
kind = "project"
active_limit = 2

[[usage_pools.memberships]]
lane = "helix"
pool_ids = ["project:helix"]
```

`host` pool IDs use the stable configured host identity, not an agent or namespace name. Add `"host_id":"build-01"` to each enabled remote lane in `dispatch-targets.jsonl`; a missing/invalid host ID or missing host pool refuses admission and never falls back to local execution. Host limits must be 8–12 inclusive. Local work uses the configured `local_pool_id`; explicit memberships compose with these implicit execution pools.

The first rollout should remain observe-only. Check scheduler logs for `USAGE_POOL` records and verify each remote target maps to one stable host pool before setting `observe_only = false`. Pool definitions, memberships, and leases are reconciled at daemon startup in the same SQLite DB; do not manually delete lease rows or move active work between host IDs. To roll back, disable usage pools in TOML and restart; existing lease history remains in the DB.

Pool counters are included in `GET /api/v1/status` as `usage_pools[]`; independent scheduler DBs are outside the shared-cap guarantee. No live DB, fleet config, target assignment, or routing change is part of this implementation.
