# ADR-002: Board Ownership Gates the Tasks-Mode Waiver

**Status:** Accepted
**Date:** 2026-09-17
**Author:** coding-hermes-scheduler foreman (SCHED-GAP-141)
**Deciders:** Bane (Alexis Okuwa)

## Context

SCHED-GAP-124 gave lanes two admission modes: `cooldown` (cron — a tick is
admitted only when `now - last_tick_completed >= effective cooldown`) and
`tasks` (work-driven — a lane whose board holds non-perpetual open work is
admitted immediately; when the board drains, including the perpetual-only
NEVER-DONE / fixture case, the cooldown pin applies again). Namespaces carry
the default; projects may override.

Measured leak (live scheduler.db, 2026-09-17): the "satellite" lanes
(`-sync`, `-qa`, `-dogfood`, `-pm`) run in their own workdirs, but each one's
board path resolves into the PRIMARY project's workdir:

```
/home/kara/.hermes/sync-workdirs/bunker-sync/.coding-hermes/board  ->  /home/kara/bunker/.coding-hermes/board
```

`.coding-hermes/` is a real directory; its `board` entry is a **symlink**.
`os.Stat` follows symlinks, so owner and satellite report the SAME inode and
`st_nlink` stays **1** for both — inode identity cannot separate them, and a
hardlink count (`nlink > 1`) is never true. Every satellite therefore read the
PRIMARY's backlog as its own "has work" signal and never fell back to its
cooldown pin:

| lane class | namespace (at the time) | cooldown pin | measured 24h |
|---|---|---|---|
| 22 × `-sync` | `duckbrain-sync` (`tasks`) | 21600s (4/day max) | 156 ticks |
| `-qa` / `-dogfood` / `-pm` | `qa`, `pm` (`tasks`) | 21600s | fast path, waived |

Bane's ruling (2026-09-17): tasks mode is *"fast when there is work, slow when
it is only perpetual stuff"*. A lane that merely READS someone else's board is
a time-based lane and must follow its cooldown timer.

## Options

### Option A: Inode / hardlink identity
Compare `(dev, ino)` across enabled projects, or treat `st_nlink > 1` as
"shared board". **Rejected:** symlinks are not hardlinks. Both paths report the
same inode with `nlink == 1`, so neither check fires — the measurement above
disproves it.

### Option B: Name/suffix heuristics (`-sync`, `-qa`, …)
Refuse the waiver for lanes whose name ends in a known suffix. **Rejected:** a
hardcoded name list in the core admission path is exactly what the GAP-124
design forbids; renaming or adding a lane class silently changes semantics.

### Option C: Namespace configuration only
Operators flip the satellite namespaces (`qa`, `pm`, `duckbrain-sync`) to
`cooldown` via API + `fleet.toml`. **Necessary but not sufficient:** it is the
right fleet-level lever and was applied, but it puts the correct behavior in
per-namespace policy rather than in the scheduler's own model, so any namespace
later re-armed to `tasks` re-opens the leak, and it cannot express "this
particular lane reads a foreign board" (e.g. a lane moved into the primary
`tasks` namespace).

### Option D (adopted): Derived ownership by realpath containment
The board file the standard board walk finds is fully symlink-resolved
(`filepath.EvalSymlinks`, applied to the board path AND to the lane's workdir)
and must live **inside that lane's own workdir**. Otherwise the lane does not
own the board it reads: the tasks-mode waiver is refused, and its wall-clock
cooldown decides. Plus two config-managed per-project overrides for the cases
the filesystem walk cannot decide:

- `board_ownership = "owner"` — this lane owns the board it reads even though
  the resolved path lies outside its workdir (generated boards, shared board
  dirs).
- `board_ownership = "shared"` — this lane reads a board it does NOT own
  (always cooldown-paced), which also covers a *copy* or bind-mounted foreign
  board the path check would pass.

**Pros:** name-free and data-derived; survives renames, new lane classes, and
namespace re-arming; the knobs ride the existing config chain (SQLite row +
`fleet.toml` + API + loader re-pin); the owning lanes' fast path is untouched.
**Cons:** the filesystem layout becomes semantically load-bearing — a lane that
copies (rather than links) another project's board needs the explicit `shared`
flag.

## Decision

Adopt **Option D**. The tasks-mode cooldown waiver fires only for a lane that
OWNS the board it reads. Ownership is derived by realpath containment; the
explicit `board_ownership` override is the config-managed exception; the
existing `admission_mode` override remains the sanctioned per-lane pacing knob.
Both are stored in the SQLite row, pinnable from `fleet.toml`, settable through
the API, and re-pinned by the loader on every start (SCHED-GAP-025/121
durability law). Everything else is unchanged: urgency/priority ordering,
namespace concurrency caps, budget caps, the post-tick pacing floor, blackout,
failure backoff, and the perpetual-only fallback (GAP-106).

## Consequences

- **Fleet effect:** the satellite lanes fall back to their cooldown pins; the
  primary lanes keep the fast path. Operators should expect the `-sync`/`-qa`/
  `-dogfood`/`-pm` tick rate to drop to the pin-derived rate (21600s ≈ 4/day
  per lane) plus the pacing floor.
- **Observability:** a refused waiver logs once per transition
  (`grep ADMISSION-OWNERSHIP:`), so "why is this tasks lane idle?" is answerable.
- **Durability:** a permanent pacing pin must exist in BOTH stores — the live
  SQLite row and the `fleet.toml` block — or the next regeneration normalizes
  it away.
- **Scope boundary:** the post-tick adaptive-cooldown escalation still keys off
  the configured mode, not off derived ownership — a tasks-namespace satellite
  keeps its floor pin instead of receiving an escalating cooldown it never
  asked for.
- **Migration:** v32 adds `projects.board_ownership` (default `''` = auto), so
  existing rows keep the derived rule with no operator action.
