# Root TOML (`--config`): which `[scheduler]` keys actually apply

When you start the daemon with `--config <file>`, the file is read twice with two
different decoders:

- `[[projects]]` / `[[namespaces]]` — declarative fleet seeding, applied at boot
  (`main.go:521`, `config.ApplyFleetConfig`).
- `[scheduler]`, `[api]`, `[usage_pools]`, `[crier]` — the root daemon layer,
  applied under the **default-guard** pattern.

This page covers the second part: which `[scheduler]` keys the daemon actually
applies, which ones are advertised by `--schema` (and exist in
`config.example.toml`) but are **silently ignored** in the root TOML, and one
key that works but is documented in no schema.

## Precedence chain

For every key in the table below the resolution order is:

```
CLI flag  >  SCHEDULER_* env var  >  root TOML  >  built-in default
```

The TOML layer uses a **default-guard**: the TOML value is applied only while
the corresponding flag still sits at its built-in default, which is what keeps
CLI and env on top. Two consequences:

1. A TOML value cannot override a value you passed on the command line or via
   the environment — even an explicitly smaller one.
2. A TOML value equal to "off"/"keep default" for that key is a no-op. For
   keys where `0` is meaningful (e.g. `tasks_pacing = "0s"` disables pacing),
   TOML *can* express off; for strictly-positive keys (`slot_patience`,
   `load_gate_threshold`, `spawn_mem_limit_mb`, the auto-disable knobs) a TOML
   `0` just leaves the default in place.

TR-162: the loader enforces the middle of this chain itself —
`config.LoadRootConfig` (the daemon boot path) applies the `SCHEDULER_*` env
layer over the decoded TOML, where it previously returned the raw decode (so
env > TOML held only on the `LoadConfig` path and in main.go's per-key
default-guard blocks). This corrects the loaded config for every call site;
it does not wire `[gateway]` / `[daemon]` / `[duckbrain]` into the running
daemon, which still read those knobs from flags/env only (see below).

Keys outside this pattern:

- `blackout_windows` has no CLI flag or env var at all — the TOML file is the
  only way to configure it, and `main.go:663` calls `loop.SetBlackoutWindows`
  directly whenever the array is non-empty.
- `id` (federation identity) resolves `SCHEDULER_ID` env > TOML `id` >
  short hostname (`main.go:486-492`); there is deliberately no CLI flag.

## Key table

| Key | Applies from root TOML? | How | Notes |
|---|---|---|---|
| `blackout_windows` | **Yes** | Direct set (no flag/env layer exists) | Advertised by **no** schema — see below. |
| `id` | Yes | env `SCHEDULER_ID` wins | No CLI flag by design. |
| `auto_disable_failure_rate` | Yes | Default-guard (`--auto-disable-failure-rate` default 0) | |
| `auto_disable_window` | Yes | Default-guard (flag default 100) | |
| `auto_disable_min_ticks` | Yes | Default-guard (flag default 50) | |
| `failure_window` | Yes | Default-guard (flag default 100) | |
| `weight_budget` | Yes | Default-guard (`--budget` default 100) | |
| `gateway_response_timeout` | Yes | Default-guard (flag default 30m) | Invalid duration logs a WARN and keeps the default. |
| `slot_patience` | Yes | Default-guard (flag default 5m) | Strictly positive only; TOML `0` = keep default. |
| `load_gate_threshold` | Yes | Default-guard (flag default 0) | Positive value arms the gate and the wave-load ceiling. |
| `tasks_pacing` | Yes | Default-guard (flag default 60s) | `0s` disables pacing explicitly. |
| `spawn_mem_limit_mb` | Yes | Default-guard (flag default 0) | Positive arms the RLIMIT_AS cap. |
| `session_silence_grace` | Yes | Default-guard (flag default 0) | Positive arms the watchdog. |
| `session_poll_loop_min_ticks` | Yes | Default-guard (flag default 0) | Value >= 6 arms the poll-loop guard at that (stricter) threshold; 1-5 rejected in TOML (the flag layer clamps them up). |
| `metered_budget_enabled` | Yes | Default-guard (env `SCHEDULER_METERED_BUDGET_ENABLED` wins, even an explicit `false`) | Not listed in `--schema` output — an undocumented-but-applied key. |
| `min_interval` | **No** | — | Advertised by `--schema` and present in `config.example.toml`, but `main.go` never reads `Scheduler.MinInterval`; only `--min-interval` / `SCHEDULER_MIN_INTERVAL` work. |
| `max_interval` | **No** | — | Same shape as `min_interval`. |
| `num_levels` | **No** | — | Same. |
| `max_concurrent` | **No** | — | Same. |
| `tick_timeout` | **No** | — | Same. |
| `namespace_mode` | **No** | — | Same. |

Also ignored from the same root file: the `[daemon]`, `[gateway]` and
`[duckbrain]` sections shown in `config.example.toml`. Only the
`[[projects]]` / `[[namespaces]]` seeding, `[scheduler]` keys above, `[api]`
(`read_timeout`, operator credentials), `[usage_pools]` and `[crier]` are
applied. Use flags or env vars for everything else.

The six ignored keys come from the pre-`LoadRootConfig` era: they were added to
the `SchedulerConfig` struct and the `--schema` output, and the flag/env path
was built, but no root-TOML application point was ever wired for them. Setting
them in the file does not error and does not warn — the daemon simply keeps the
flag/env value.

## `blackout_windows` example

Blackout windows slow the fleet (cooldown multiplier) or skip projects
entirely during peak-pricing hours. Times are `HH:MM` **UTC**; with
`weekdays_only = true` the weekday check is evaluated in Beijing time (UTC+8)
to match the provider's weekend pricing rule.

```toml
[scheduler]
# Double every cooldown during 01:00–04:00 UTC, Mon–Fri.
[[scheduler.blackout_windows]]
start = "01:00"
end = "04:00"
multiplier = 2.0
weekdays_only = true

# Skip scheduling entirely 06:00–10:00 UTC, every day (multiplier 0).
[[scheduler.blackout_windows]]
start = "06:00"
end = "10:00"
multiplier = 0.0
```

On boot the daemon logs `Blackout: loaded N windows` when any window applies.
Take effect requires a daemon restart after editing the file.

## The `--show-config` caveat

`--show-config` prints the resolved **CLI + env** layers only and exits
immediately — its early exit sits before the root-TOML application block, so
values that came from the TOML file (and `blackout_windows`, which has no flag
or env representation) do **not** appear in its output. Do not use
`--show-config` to verify TOML-applied settings; check the boot log lines
instead (`Blackout: loaded N windows`, `TASKS-PACING: set from config`,
`LOAD-GATE: enabled from config`, `ADV-R11: spawn mem limit enabled from
config`, etc.).

## Verified against

All claims in the table were re-derived from the code in this tree
(cmd/schedulerd/main.go @ worktree HEAD):

- Applied keys, default-guard pattern: `cmd/schedulerd/main.go:775-797`
  (auto-disable knobs, failure window, weight budget), `:800-810`
  (gateway_response_timeout), `:843-856` (slot_patience), `:857-871`
  (load_gate_threshold), `:873-879` (metered_budget_enabled), `:879-895`
  (tasks_pacing), `:896-905` (spawn_mem_limit_mb), `:905-918`
  (session_silence_grace).
- `blackout_windows`: `cmd/schedulerd/main.go:659-666` — `rootCfg.Scheduler.BlackoutWindows`
  → `loop.SetBlackoutWindows` (direct set, no flag/env layer).
- `id`: `cmd/schedulerd/main.go:483-492` — `SCHEDULER_ID` env > TOML > hostname.
- Ignored keys: `internal/config/config.go:51-62` defines
  `MinInterval`/`MaxInterval`/`NumLevels`/`MaxConcurrent`/`TickTimeout`/`NamespaceMode`
  (plus `BlackoutWindows`) on `SchedulerConfig`; the only reads of those flag
  variables in `main.go` are the flag declarations (`:44-50`) and uses of the
  flag values (`:334-336`, `:582`, `:593`, `:1109-1126`, `:1184`) — no
  `rootCfg.Scheduler.MinInterval`-style read exists anywhere in the tree.
- `--show-config` early exit before the TOML block: `cmd/schedulerd/main.go:332`
  vs the TOML application block starting at `main.go:770`.
- `--schema` advertising (including the six ignored keys) but not
  `blackout_windows` / `metered_budget_enabled`: `cmd/schedulerd/show_config.go:35-53`.
- `config.example.toml` carrying the six ignored keys (`min_interval` …
  `namespace_mode`, lines 27-41) with no blackout example: repo root,
  `config.example.toml`.
