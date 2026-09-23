# The fleet config model — what controls how often a lane runs

**Offline copy:** scheduler repo `docs/fleet-config-model.md` · **As of:** 2026-09-20 (UTC) · **Verified against:** the live daemon on `127.0.0.1:9090`, `~/.hermes/coding-hermes/scheduler.db`, `~/.hermes/fleet.toml`, and the source at `83f5c824`.

This page answers one question — *how often does a lane run, and what controls it* — and it answers it the only way this fleet accepts: **every claim below is paired with a command that shows the claim is true.** If a command in this page does not run on your host or does not print what the page says, the page is wrong, not the fleet. Section 8 is the index of every command.

Every knob gets the same four parts: **what it does / where it physically lives / how to change it safely / the command that verifies the change took.**

---

## 1. The short answer

A lane runs when **all** of the following are true at the same instant an evaluation happens:

1. **Its mode permits admission now.** In `cooldown` mode that means `now − last_tick_completed ≥ cooldown_s` (the wall clock). In `tasks` mode the wall-clock gate is *waived* while the lane's **own** board holds non-perpetual pending work — with one exception: a lane only gets that waiver if it **owns** the board it reads (§3).
2. **Even then, a 60-second pacing floor applies** to `tasks` lanes after *any* terminal tick (plus up to 20 % jitter), so consecutive tasks ticks never fire at 0 ms.
3. **Nothing escalated the cooldown.** If a lane is *armed* for adaptive cooldown, each no-progress tick past the threshold **doubles** `cooldown_s`, up to a ceiling of 8 × floor. Any progress (a commit, or a net drop in open board rows) snaps it straight back to the floor. **Nothing slows a lane down except this escalator, and nothing speeds it up except a landing of real work.**
4. **A slot + budget + host are free.** The global pool (10), the lane's namespace cap (8 for foremen, 1 for each satellite family), the weight budget (100), and the load gate (defers at loadavg ≥ 12) can each *defer* a spawn. Deferral costs latency only — it never consumes a tick and never damages a cooldown.
5. **It wins the ordering.** Four urgency tiers (organic, pending-board-boost, bump, starvation) decide *which* eligible lane gets the scarce slot. Ordering is **not** admission: the board wake's pending boost re-orders the queue and calls for a re-evaluation, but it **cannot shorten a cooldown** — no code path skips a spawn on board state alone.

The single sentence version: **the cooldown pin is the minimum spacing; the escalator is the only thing that makes it slower; the caps and gates only ever make it later; the board boost only re-orders who goes first.**

Two things select the whole shape, and they are the two most common sources of confusion:

- **Admission mode** — namespace default, per-project override (§3).
- **Which store you wrote to** — the live DB is what the daemon reads, `fleet.toml` is what a restart re-pins from, and a change living in only one of them is drift, not policy (§5).

```bash
# The two mode dials and the global ceiling, in one shot:
curl -s 127.0.0.1:9090/api/v1/config | jq '{max_concurrent, namespace_mode, tasks_pacing, load_gate_threshold, min_interval}'
```

Expected (2026-09-20):

```json
{
  "max_concurrent": 10,
  "namespace_mode": true,
  "tasks_pacing": "1m0s",
  "load_gate_threshold": 12,
  "min_interval": "30s"
}
```

---

## 2. The three dials

### 2.1 Global concurrency — `--max-concurrent` = **10**

| Part | Value |
|---|---|
| **What** | The hard ceiling on ticks running at once across the *whole* fleet, in every namespace. `SlotPool` is constructed with it; the packer also breaks out of selection at it. |
| **Where** | **Systemd user unit** `~/.config/systemd/user/coding-hermes-scheduler.service`, `ExecStart` flag `--max-concurrent 10`. **Code default 10** at `cmd/schedulerd/main.go:41`. An env layer exists (`SCHEDULER_MAX_CONCURRENT`, read in `internal/config/loader.go:225`) but that loader (`LoadConfig`) has **no non-test caller** in this tree — do not rely on it for the daemon. `~/.hermes/fleet.toml` does **not** carry it (it is a namespace/project file), and `[scheduler] max_concurrent` in a root TOML is **not read by the daemon** — `main.go` reads only blackout windows, auto-disable knobs, gateway timeout, slot patience, weight budget, load gate, tasks pacing and spawn mem limit from the root file. |
| **Wins on restart** | The systemd unit. It is argv; nothing re-pins it. |
| **How to change** | Drain first (`docs/runbook-drain-restart.md`), edit `--max-concurrent` in the **user** unit, `systemctl --user daemon-reload && systemctl --user restart coding-hermes-scheduler.service`. A dormant **system** unit also exists under `/etc/systemd/system/` — editing it changes nothing; the live daemon is the user one. |
| **Verify** | Command **A1** (`ExecStart`) and **A2** (`/api/v1/config`). The API value is the resolved snapshot the running process actually holds, so it is the authority for "what is live now". |

```bash
# A1 — what the running unit was started with
systemctl --user show -p ExecStart coding-hermes-scheduler.service | tr ';' '\n' | grep -o -- '--max-concurrent [0-9]*'
# A2 — what the process resolved (the API snapshot)
curl -s 127.0.0.1:9090/api/v1/config | jq .max_concurrent
# A3 — the code default, if the flag is absent from a unit
grep -n 'flag.Int("max-concurrent"' cmd/schedulerd/main.go
# A4 — the env layer for this knob exists but is unreachable from the daemon
grep -rn 'config.LoadConfig' --include=*.go . | grep -v _test
```

A1 → `--max-concurrent 10` · A2 → `10` · A3 → `41: maxConcurrent := flag.Int("max-concurrent", 10, "Max concurrent foremen")` · **A4 → no output, exit 1.** A4 is the one command here whose *expected* result is a non-zero exit: the only match in the tree is the function declaration itself, so an empty result is what proves no caller outside tests exists. Read it as "the env layer is declared but unreachable from the daemon", not as a broken command.

`--namespace-mode` must be on for the per-namespace caps to exist at all; A2 shows it as `true` on the live fleet. Without it the global dial is the only cap.

### 2.2 The foremen namespace — `coding-hermes` `max_concurrent` = **8**

### 2.3 Each satellite namespace — `max_concurrent` = **1**

The two are the same knob at different scope, so they get one table.

| Part | Value |
|---|---|
| **What** | `max_concurrent` on a **namespace**: how many ticks that namespace may have in flight at once. `0` = unlimited (the global cap still applies). At 1 per satellite family, no satellite family can hold more than one global slot, which is what guarantees the foremen room. |
| **Where** | **Primary:** the `namespaces` row in `~/.hermes/coding-hermes/scheduler.db` — this is what the packer reads. **Restart pin:** the `[[namespaces]]` block in `~/.hermes/fleet.toml`, re-applied by `internal/config/loader.go` (`ApplyFleetConfig`). **Code default** for a namespace created with no key: `defaultMaxConcurrent = 8` (`internal/config/loader.go:53`) — a *library* default, never the fleet's current value. |
| **Wins on restart** | The **`fleet.toml` value, when the entry carries a non-zero `max_concurrent`** (SCHED-GAP-149). A keyless entry — or an explicit `0` — leaves the live DB cap alone, so a cap assigned through the API survives a restart with a keyless entry. A negative value normalizes to `0`, so the same file cannot land two different caps depending on whether the row pre-existed. |
| **How to change** | (1) `PUT /api/v1/namespaces/{id}` with `{"max_concurrent": N}` — live immediately. (2) Make the durable store carry it: mirror the DB into `~/.hermes/fleet.toml` (that file's writer, §5). (3) Re-read both stores with **B1**/**B2**/**B3** below. |
| **Verify** | **B1** (live API), **B2** (the DB rows the packer actually reads), **B3** (the restart pin). All three must agree for a value to be durable. |

```bash
# B1 — live: id, cap, admission mode, load-gate opt-out
curl -s 127.0.0.1:9090/api/v1/namespaces | jq -r '.namespaces[] | "\(.id)\t\(.max_concurrent)\t\(.admission_mode)\t\(.load_gate)"'
# B2 — the DB rows behind B1
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT id, max_concurrent, admission_mode, COALESCE(load_gate,'') FROM namespaces ORDER BY id;"
# B3 — the restart pin for the foremen namespace
grep -A6 '^id = "coding-hermes"' ~/.hermes/fleet.toml | head -7
# B4 — how many namespaces are at cap 1, and the sum of all caps vs the global 10
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT count(*) FROM namespaces WHERE max_concurrent=1;"
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT 8 + (SELECT count(*) FROM namespaces WHERE max_concurrent=1 AND id <> 'coding-hermes');"
# B5 — the six-namespace satellite list the checker asserts caps on
grep -n 'SATELLITE_NS = ' ops/check-fleet-invariants.py
```

Live output (2026-09-20) — B1 and B2 agree exactly:

```
backup            0  cooldown
coding-hermes     8  tasks
data-cleanup      0  cooldown
doc-writer        1  cooldown
dogfood           1  cooldown
duckbrain-infra   0  cooldown
duckbrain-sync    1  cooldown
monitoring        0  cooldown
pm                1  cooldown   load_gate=off
qa                1  cooldown
releases          1  cooldown
update-and-patch  1  cooldown
```

B3 → `max_concurrent = 8`, `admission_mode = "tasks"`. B4 → `7` and `15`. B5 → `70:SATELLITE_NS = ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer")`.

**Read this table carefully — the cap column is a policy, not a uniform rule.** The row's "satellite = 1" applies to the six satellite-family namespaces (`qa`, `pm`, `dogfood`, `duckbrain-sync`, `releases`, `doc-writer`) plus `update-and-patch` — seven namespaces at cap 1, verified by command **B4**. Four namespaces (`backup`, `data-cleanup`, `duckbrain-infra`, `monitoring`) are deliberately `0` = unlimited, and each holds low-frequency infrastructure lanes. And the caps are a *ceiling*, not a target: with 8 for the foremen plus 1 for each of those seven, the sum of caps (15) deliberately exceeds the global 10 — so the packer and the weight budget, not this table, decide how many actually run. The invariant checker asserts exactly the 8/1 shape and its six-namespace satellite list (command **F1**/**F6**).

---

## 3. The two admission modes

`admission_mode` exists at two levels and resolves **project → namespace → `cooldown`**: a per-project override wins; an unknown namespace id or an empty mode on both levels falls back to `cooldown`. An empty project value means *inherit*, not *off*, and that is the state of all 316 project rows on the live fleet.

| Mode | Meaning | Right for |
|---|---|---|
| `cooldown` (default) | Wall-clock cron semantics: admit only when `now − last_tick_completed ≥ cooldown_s`. | Every satellite lane. Their work is cadence-driven, and their cadence is the signal an operator reads. |
| `tasks` | Admit immediately while the project's **own** board holds real (non-perpetual) pending work; fall back to the wall-clock pin once the board is drained or holds only fixture rows. All other gates (caps, budget, pacing, blackout, failure backoff) still apply. | The `coding-hermes` foremen — "fast when there is work, slow when it is only perpetual stuff". |

### 3.1 The board-ownership precondition — the one rule that explains the satellites

**A namespace may only be `tasks` if it owns the board it admits on.** Ownership is *derived, never named*: the board file found by the standard board walk is fully symlink-resolved, and it must live **inside the lane's own (also resolved) workdir**. A board reached through a link into another lane's workdir is not owned by this lane, so the cooldown waiver is refused and the wall-clock pin decides — exactly as if the lane were in `cooldown` mode.

**Why this exists (measured 2026-09-17, live; the measurement is recorded in the source comment, command C5):** every satellite lane ships a real `.coding-hermes/` directory whose `board` entry is a **symlink** to its primary's board directory. Before the ownership rule, the board walk resolved through that link, so a satellite read its primary's backlog as its *own* "has work" signal and never fell back to its cooldown pin — 22 `-sync` lanes produced 156 ticks in 24 h against a 6 h pin (4/day maximum). That is the whole reason satellites fire fast when something is wrong, and the reason `SATELLITE_NS` in the invariant checker exists.

Two facts worth internalising, because they are the reason a name- or inode-based check will not work:

- **`os.Stat` follows the symlink, so owner and satellite report the same `(device, inode)` and `st_nlink` stays 1 for both.** Inode identity cannot separate them, and a hardlink count is never > 1. Only `filepath.EvalSymlinks` on both the board and the workdir plus containment actually discriminates.
- **Ownership is fail-closed.** An empty workdir, a missing board, a dangling link or an unresolvable path all report *not owned* — which refuses the waiver. Refusing costs latency; granting it wrongly is the leak. Every refusal logs a grep-able `ADMISSION-OWNERSHIP:` line naming the workdir.

The explicit override is `projects.board_ownership` (`''` = auto/derived, `owner` = assert ownership, `shared` = assert a foreign board, always cooldown-paced). It is API-settable and pinned from `fleet.toml` like every other pin. All 316 live rows are `''` (auto).

```bash
# C1 — the satellite symlink that causes all of this (any -pm/-qa/-sync/-dogfood workdir)
readlink -f /home/kara/crier/.coding-hermes/board
readlink -f /home/kara/.hermes/stand-in/pm/crier/.coding-hermes/board
# C2 — the schema for both fields (defaults, CHECK constraints)
grep -n 'admission_mode TEXT\|board_ownership TEXT' internal/database/migrations.go
# C3 — the ownership rule itself, and its fail-closed branches
grep -n 'func laneOwnsBoard' -A 8 internal/scheduler/admission_mode.go
# C4 — every project row's mode and ownership (empty = inherit / auto)
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT COALESCE(NULLIF(admission_mode,''),'(inherit)'), COALESCE(NULLIF(board_ownership,''),'(auto)'), count(*) FROM projects GROUP BY 1,2;"
# C5 — the live measurement that motivated the ownership rule
grep -n '22 -sync' -A 2 internal/scheduler/admission_mode.go
```

C1 → both paths resolve to `/home/kara/crier/.coding-hermes/board` — the PM lane's board *is* crier's board, so the PM lane is time-based by construction and its 24 h pin is what paces it.

C2 → `417: ALTER TABLE namespaces ADD COLUMN admission_mode TEXT NOT NULL DEFAULT 'cooldown' CHECK(admission_mode IN ('cooldown','tasks'));` — the namespace default is `cooldown` **at the schema level**, so a namespace with no value is timer-paced by luck of the DDL, which is why the checker flags an empty namespace mode rather than assuming it.

C4 → `(inherit)|(auto)|316`.

### 3.2 How to change an admission mode

1. Decide the namespace default. Namespace-level `admission_mode` is a legitimate `tasks` **only** for a namespace whose projects own their boards — the exception is precisely the satellite families, which must be `cooldown`.
2. `PUT /api/v1/namespaces/{id}` `{"admission_mode": "cooldown"|"tasks"}`. Live immediately.
3. If a single lane needs a different mode than its namespace, set the project override: `PUT /api/v1/projects/{name}` `{"admission_mode": "..."}`. The project value wins over the namespace value.
4. Mirror to the restart pin (that file's writer, §5) — both the `[[namespaces]]` and the `[[projects]]` block.
5. Verify with **C4** and the checker (**F1**).

The loader pins these only when `fleet.toml` explicitly sets them: an API-flipped mode **survives a restart** with a keyless entry, and an invalid value logs a warning and is skipped rather than crashing the boot. That makes `admission_mode` the one dial where a DB-only change is not automatically lost — which is exactly why the invariant checker reads the DB directly rather than trusting the file.

---

## 4. The cooldown law

### 4.1 The floor — a uniform 6-hour minimum

`cooldown_s` is the minimum spacing between a lane's terminal tick and its next admission. The fleet law is a **6 h floor (21600 s)** for every enabled lane, with two documented exceptions tracked as named tiers:

| Tier | Value | Where it lives |
|---|---|---|
| `qa-audit` | 86400 (24 h) | `COOLDOWN_TIERS` in `ops/check-fleet-invariants.py` |
| `release-engineer` | 604800 (7 d) | `COOLDOWN_TIERS` in `ops/check-fleet-invariants.py` |
| pm **family** | 86400 (24 h) | `SATELLITE_FAMILY_PINS` — applies to every `-pm` lane |
| dogfood **family** | 259200 (72 h) | `SATELLITE_FAMILY_PINS` — applies to every `-dogfood` lane |
| qa **family** | 43200 (12 h) | `SATELLITE_FAMILY_PINS` — applies to every `-qa` lane |
| sync **family** | 43200 (12 h) | `SATELLITE_FAMILY_PINS` — applies to every `-sync` lane |

**Correction to the source row:** the row lists "qa-audit 24 h, release-engineer 7 d, dogfood 72 h, pm 24 h" as one set. They are **two different constants with different scopes and different enforcement**. `qa-audit` and `release-engineer` are *named projects* in `COOLDOWN_TIERS`, whose check is "this project must be exactly this value". `dogfood`/`pm` (and `qa`/`sync`) are *name-suffix families* in `SATELLITE_FAMILY_PINS`, whose check is "every enabled lane matching this suffix must have **both** `cooldown_s` and `cooldown_floor_s` equal to the family value". A project tier is one row; a family pin is a class of dozens of rows. The distinction matters the moment you change one.

**Why both fields are checked for a family:** a satellite reads its primary's board through the symlink — permanently "has work" — so the policy's REDUCE rule would pull it to the 6 h default on every run. Pinning the family is what keeps a satellite's cadence readable. `cooldown_floor_s` is checked alongside because either field alone is enough to make the cadence unreadable (§4.3).

```bash
# D1 — the floor and both tier tables, straight from the gate
grep -n 'COOLDOWN_FLOOR = \|COOLDOWN_TIERS = \|SATELLITE_FAMILY_PINS = ' ops/check-fleet-invariants.py
# D2 — every enabled lane's cooldown, as a histogram
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT cooldown_s, count(*) FROM projects WHERE enabled=1 GROUP BY cooldown_s ORDER BY cooldown_s;"
# D3 — any enabled lane below the 6h floor (should be an empty set)
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT name, cooldown_s, cooldown_floor_s FROM projects WHERE enabled=1 AND cooldown_s < 21600;"
```

D1 → `71: COOLDOWN_FLOOR = 21600`, `73: COOLDOWN_TIERS = {"qa-audit": 86400, "release-engineer": 604800}`, `78: SATELLITE_FAMILY_PINS = {"qa": 43200, "pm": 86400, "sync": 43200, "dogfood": 259200}`.

D2 (2026-09-20, 156 enabled lanes):

```
900|2        21600|90      43200|13      86400|26      259200|24      604800|1
```

D3 → `hermes-canopy|900|21600` and `coding-hermes-tools|900|21600` — **two live exceptions**, not an empty set. See §7; both are filed as findings, not fixed here.

### 4.2 The anti-snap rule — speeding up needs a dated ruling, slowing down does not

The policy's `ELEVATED_PINS` dictionary is the **only** place an operator pin that must survive normalization can live. Its rule is directional:

- **Faster than the baseline = requires a dated owner ruling, recorded in `ELEVATED_PINS`.** Every entry carries the ruling's date and reason in a comment. Without an entry, the policy's own correction rules revert a below-fast live value to the file's pin, or raise a sub-floor value to the fast tier — i.e. a fast value that exists only in the live DB is *wake residue*, not intent, and gets normalized away.
- **Slower is always allowed.** Nothing in the policy promotes a lane *faster*; the promotion path only ever moves a lane to the idle tier or re-applies an operator pin. Slowing a lane down costs throughput and needs no permission; speeding one up spends money the fleet was deliberately throttled not to spend.

The mechanism that makes this directional is a hard-skip, not a heuristic: a project in `ELEVATED_PINS` **skips the entire evaluation** — no reduce, no wake-revert, no promotion race — and the pin is written to `fleet.toml` by the writer so restarts re-pin to it. A pin that sits outside the canonical policy set (900/21600/43200/7200) is *also* honoured as operator intent, which is how a weekly 604800 cadence survives; the `ELEVATED_PINS` entry is what makes a **fast** value survive.

```bash
# D4 — the whitelist, with each ruling's date and reason
grep -n 'ELEVATED_PINS = {' -A 10 ~/.hermes/scripts/fleet-cooldown-policy.py
# D5 — the canonical policy set, and that a non-canonical pin is honoured rather than normalized
grep -n '3600s fast (1h) / 21600s default (6h) / 43200s idle (12h)' ~/.hermes/scripts/fleet-cooldown-policy.py
grep -n 'Any fleet.toml pin outside the canonical policy set' -A 3 ~/.hermes/scripts/fleet-cooldown-policy.py
```

Live (2026-09-20) — 9 entries, 5 carrying their own dated ruling comment, the four h3 SDK/shim entries inheriting the dated h3 rationale directly above them:

```
h3                       43200   Bane 2026-08-27: h3 family ticks too fast — 12h anti-flood pin
h3-sdk-go-foreman        43200
h3-sdk-python-foreman    43200
h3-sdk-typescript-foreman 43200
h3-shim-foreman          43200
warpfs                   21600   Bane 2026-08-19: ALL projects to 6h window for now
hermes-canopy-releng     86400   Bane 2026-09-19: releng is 24h, not 6h
hermes-dagger              900   Bane 2026-09-15: 15min dagger speed ruling
hermes-canopy            21600   Bane 2026-09-17: all coding-hermes primaries at 21600
```

### 4.3 The escalator (adaptive cooldown) and the ceiling

Adaptive cooldown exists to slow a lane that stops producing. Three facts decide whether it can affect a lane at all:

- **It only runs on armed lanes.** `adaptive_cooldown` is a per-project flag. The fleet default is **disarmed**, and the writer arms only the `coding-hermes` namespace — *and only when that namespace is timer-based*, because a `tasks`-admission lane spawns from board state and arming it is dead bookkeeping. On the live fleet **0 of 156 enabled lanes are armed** (command **E2**). Every armed row on this fleet belongs to a *disabled* project.
- **The escalation is `×2` per no-progress tick past the threshold** (default `no_progress_threshold = 10`), capped at `cooldown_ceiling_s`. An explicit ceiling always wins verbatim; when none is set the ceiling is **derived as 8 × `cooldown_floor_s`** — one authority, never a second hardcoded constant. That is why the fleet's ceiling values are exact multiples of their floors (172800 = 8 × 21600, 345600 = 8 × 43200, and so on).
- **Progress is the reset.** Any committed code change or net decrease in open board rows resets the streak and snaps `cooldown_s` back to the floor immediately. A floorless row (floor 0 and ceiling 0) tracks the streak for observability but never escalates.

**A `tasks`-mode namespace keeps its floor-pin cooldown even though it is never armed** — ownership gates the *waiver*; it does not hand a time-based lane an escalating cooldown it never asked for.

```bash
# E1 — the two constants behind every ceiling in the fleet
grep -n 'AdaptiveCeilingFloorMultiplier = \|DefaultAdaptiveCooldownThreshold = ' internal/database/models.go
# E2 — how many enabled lanes are actually armed
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT count(*) FROM projects WHERE enabled=1 AND adaptive_cooldown=1;"
# E3 — the ceiling histogram (each value should be 8 × a floor in D2)
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT cooldown_ceiling_s, count(*) FROM projects WHERE enabled=1 GROUP BY 1 ORDER BY 1;"
# E4 — one row, all four cooldown columns, from the live API
curl -s 127.0.0.1:9090/api/v1/projects/hermes-canopy | jq '.project | {cooldown_s, cooldown_floor_s, cooldown_ceiling_s, adaptive_cooldown, no_progress_threshold}'
# E5 — the escalator's own rule (×2 per no-progress tick past the threshold)
grep -n 'adaptiveCooldownFactor = 2' internal/scheduler/adaptive_cooldown.go
# E6 — how many enabled lanes exist at all (the denominator for E2)
sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT count(*) FROM projects WHERE enabled=1;"
```

E1 → `28: DefaultAdaptiveCooldownThreshold = 10`, `39: AdaptiveCeilingFloorMultiplier = 8`. E2 → `0`. E3 → `0|26`, `172800|18`, `345600|64`, `691200|24`, `2073600|24`. E4 → `{"cooldown_s": 900, "cooldown_floor_s": 21600, "cooldown_ceiling_s": 172800, "adaptive_cooldown": false, "no_progress_threshold": 10}`. E5 → `79: const adaptiveCooldownFactor = 2`. E6 → `156` — so E2's `0` is 0 of 156, not 0 of a handful.

### 4.4 The other things that change *when* a lane runs

These do not touch `cooldown_s`, but an operator asking "how often does it run" is asking about them too. Each is deferral or ordering only.

| Knob | Live value | Lives in | Change it | Verify |
|---|---|---|---|---|
| **Tasks post-tick pacing** | 60 s (plus ≤ 20 % jitter) | `--tasks-pacing` flag, `cmd/schedulerd/main.go:54` (library default is `0` = off; the *shipped binary* is what paces) | edit the unit flag, drain-restart | `curl -s 127.0.0.1:9090/api/v1/config \| jq .tasks_pacing` |
| **Weight budget** | 100 | `Loop`, from `[scheduler] weight_budget` < `SCHEDULER_BUDGET` < `--budget`; provenance reported as `budget_source` | prefer the flag/unit; read back `budget_source` | `curl -s 127.0.0.1:9090/api/v1/config \| jq '{weight_budget, budget_source}'` → `{"weight_budget":100,"budget_source":"flag-default"}` |
| **Load gate** | 12 (armed) | `--load-gate-threshold` flag / `SCHEDULER_LOAD_GATE_THRESHOLD` (systemd drop-in) / `[scheduler] load_gate_threshold`; threshold `0` = disabled | flag or drop-in, then drain-restart | `curl -s 127.0.0.1:9090/api/v1/config \| jq .load_gate_threshold` → `12`; `uptime` shows the same 1-min loadavg the gate samples (live 2026-09-20: `load average: 10.71, …` — below the gate, so nothing is being deferred) |
| **Per-namespace load-gate opt-out** | only `pm` | `namespaces.load_gate = 'off'` | `PUT /api/v1/namespaces/{id}` `{"load_gate":"off"}` (only `off` or empty is valid) | `curl -s 127.0.0.1:9090/api/v1/namespaces \| jq -r '.namespaces[] \| select(.load_gate != "") \| .id'` → `pm` |
| **Blackout windows** | 2× cooldown during 01:00–04:00 and 06:00–10:00 | `[scheduler] blackout_windows` in the **root** section of the fleet file (the only root key family the daemon reads from that file besides the auto-disable/gateway/memory knobs) | edit the file, restart | `grep -A 4 '^blackout_windows' ~/.hermes/fleet.toml` |
| **Board-wake ordering boost** | pending rows ≥ 1 → urgency `5×10¹¹`; bump → `+10⁶`; starvation → `10¹²` | `internal/scheduler/board_awareness.go`, `fairness.go` | **not a knob** — it is policy; tier values are documented, not tunable | `grep -n 'pendingBoostUrgency = \|bumpBoostUrgency = \|pendingBoostMaxCount = ' internal/scheduler/board_awareness.go` |

The board wake itself: a watcher polls enabled lanes' board files every **60 s** and arms a per-project wake that fires about **5 minutes** later (bounded — later writes never extend an armed wake), then forces an evaluation. It **orders** and **triggers**; it never admits. Wall-clock cooldown remains the sole admission authority.

```bash
# G1 — the wake's two timing constants
grep -n 'boardWakeDebounce = \|boardWakePollInterval = ' internal/scheduler/board_wake.go
# G2 — the ordering tiers
grep -n 'pendingBoostUrgency = \|bumpBoostUrgency = \|pendingBoostMaxCount = ' internal/scheduler/board_awareness.go
# G3 — the two knobs the wake does NOT touch, for contrast
curl -s 127.0.0.1:9090/api/v1/config | jq '{tasks_pacing, load_gate_threshold}'
# G4 — the starvation tier, in the fairness file
grep -n 'starvationBoostUrgency = 1e12' internal/scheduler/fairness.go
```

G1 → `57: boardWakePollInterval = 60 * time.Second`, `64: boardWakeDebounce = 5 * time.Minute`. G2 → `99: pendingBoostUrgency = 5e11`, `103: pendingBoostMaxCount = 1000`, `111: bumpBoostUrgency = pendingBoostUrgency + 1e6`. G3 → `{"tasks_pacing":"1m0s","load_gate_threshold":12}`. G4 → `42: starvationBoostUrgency = 1e12`.

---

## 5. The two stores and the durability law

Two stores hold this config, they have different jobs, and **a permanent change must exist in both**.

| | Live DB — `~/.hermes/coding-hermes/scheduler.db` | Restart pin — `~/.hermes/fleet.toml` |
|---|---|---|
| **Holds** | The values the running daemon actually reads: every namespace row, every project row. | Project and namespace blocks the loader re-applies at boot. |
| **Authority** | The **live** state. It is what `/api/v1/*` serves and what the packer reads at selection time. | The state the daemon is **re-pinned to on restart**. It is not read at selection time. |
| **Change path** | `PUT /api/v1/projects/{name}`, `PUT /api/v1/namespaces/{id}`, or (guarded operations only) direct `sqlite3`, per `AGENTS.md`. | Its writer — never by hand. |
| **Failure mode if you write only here** | A restart re-pins from the file and your change is **gone**. This is SCHED-GAP-121's shape: an owner ruling that lived only in the DB while the file's writer kept emitting the old value. | Whatever the file says may be re-normalized by the writer's own correction rules before your change is ever visible, so a file-only edit can look effective and never take. |

### The law

> **Every permanent change must exist in BOTH the live DB row and the `fleet.toml` block.** A value present only in the DB is drift that the next restart re-pins away; a value present only in `fleet.toml` may be normalized back by the writer unless the project is carrying an operator pin.

### The concrete failure this prevents (SCHED-GAP-121)

An owner ruling set `hermes-dagger` to **900 s / 15 minutes**. It was applied to the live DB row *only*. The policy's `OPERATOR_7200` map still carried the old 7200, so every regeneration rewrote 7200 into `fleet.toml` — and the *next* regeneration read the file back and rewrote 7200 into the DB. Every health surface stayed green while the ruling silently reverted. It is durable today only because the ruling now lives in `ELEVATED_PINS` as `hermes-dagger: 900`, so the writer emits the canonical 900 instead of fossilising a stale value. **The file alone is not durability; the writer's own map is part of the durable form.**

### The live proof that the two stores can disagree

```bash
# S1 — the DB value (what the daemon uses)
curl -s 127.0.0.1:9090/api/v1/projects/hermes-canopy | jq '.project.cooldown_s'
# S2 — the file value (what a restart would re-pin to)
grep -A6 'name = "hermes-canopy"' ~/.hermes/fleet.toml | grep cooldown_s
```

S1 → `900`. S2 → `cooldown_s = 21600`. That is drift, this minute, on the live fleet — and both gates name it (§6). It is filed as a finding in the change report for this page rather than fixed here (fixing live fleet config is not a documentation change).

### Which writer produced the current file — check, do not assume

More than one writer exists on disk, so "the file is generated" is not a complete answer. What is checkable is what each writer *would* emit right now:

```bash
# S3 — what the decision-free DB→file mirror would write for that project
python3 ~/.hermes/scripts/fleet-sync.py 2>/dev/null | grep -A6 'name = "hermes-canopy"' | grep cooldown_s
# S4 — the mirror's own summary line (dry-run; add --write to mutate)
python3 ~/.hermes/scripts/fleet-sync.py 2>&1 >/dev/null | tail -1
```

S3 → `cooldown_s = 900`. S4 → `# dry-run: 156 projects, 12 namespaces — rerun with --write`.

**Read that against S2: the file on disk says 21600, while the decision-free mirror would emit 900.** So the mirror is *not* what wrote the current file — the operator-pin-aware writer did. That is a real operational hazard, and it is filed as a finding: `~/.hermes/fleet.toml` carries a header naming one writer while a superseding mirror exists, so an operator changing a pin must know **which** writer runs next or the change is a coin flip. Never hand-edit the file; make the two stores agree and then run the gate.

---

## 6. The two gates

### 6.1 `ops/check-fleet-invariants.py` — the live-config checker

Read-only, exit 0 = every invariant holds, exit 1 = at least one violation printed as `VIOLATION <class> <subject>: <detail>`. It exists because the fleet's shape **is config and config has no unit test** — nothing fails when it drifts back.

Its classes (`CHECK_CLASSES`): `caps`, `admission`, `cooldown`, `executors`, `workdirs`, `adaptive`, `boards`, `coverage`, `family-floor`, `targets`, `parity`, `board-vocab`, `board-legacy-status`, `board-content-dup`, `sync-orientation`, `event-id-ascending`. In one line each: the global/namespace caps and satellite cap shape; `tasks` only for the foremen and `cooldown` for every satellite; no enabled lane below the 6 h floor unless it is a documented tier; no enabled lane driving a retired driver script; every enabled workdir exists; no satellite armed for adaptive cooldown; a satellite's board actually resolves; every primary carries all four satellite lanes; every satellite sits on its family pin; every satellite's target exists and is enabled; DB↔`fleet.toml` parity for `cooldown_s`/`cooldown_floor_s`/`cooldown_ceiling_s`/`board_ownership` (projects) and `admission_mode`/`max_concurrent` (namespaces); the three board checks (writer vocabulary, legacy closed spellings, content duplicates); and `sync-orientation` — every enabled `*-sync` lane's workdir carries a non-empty `README.md` that names its target DuckBrain namespace and a findable consumption contract (the companion `<base>-sync-data` skill under the skills root, or a README pointer to one). A `*-sync` workdir with no workdir at all is check 5's fact, so `sync-orientation` stays silent there; a disabled lane is exempt; and the class skips silently when the skills root is absent (`--skills-root`, default `~/.hermes/skills`), so a CI runner never has to carry live fleet state. `event-id-ascending` (SCHED-GAP-206) asserts that every INTEGER `events.jsonl` id sits on the board's 19-digit epoch-nanosecond scale and never descends below the highest id already in the file, naming the line and the id — the class that turns the 2026-09-22 surprise (two epoch-microsecond lines that made the live `boardctl validate` FAIL with "ids must ascend") into a test. The log's PRE-SCALE prefix (ids before its first `>= 10^18` id — 979 of them on this board) and duplicate ids are tolerated exactly as `boardctl validateEvents` tolerates them, a log with no 19-digit id at all is entirely off-scale and every int id in it fires, and the events file resolves from `--events`, else the sibling of `--board`, else the same script walk-up; a board directory with no events file skips the class silently (checks 8/9's shape).

```bash
# F1 — the live fleet gate
python3 ops/check-fleet-invariants.py
# F2 — the CI shape: board checks only, no DB or TOML needed
python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only
# F3 — machine-readable, plus the classes it actually ran
python3 ops/check-fleet-invariants.py --json | jq '{ok, n: (.violations|length), checks}'
```

Live result (2026-09-20, F1, exit 1):

```
FAIL — 94 violation(s) across 12 namespaces / 158 enabled lanes
```

by class: `family-floor 52`, `sync-orientation 32`, `targets 4`, `coverage 3`, `cooldown 2`, `parity 1`. **How to read this honestly:** the checker is green on the caps, admission-mode, executor, workdir, adaptive-arming, board-present and board-vocabulary classes, and red on six others — the 52 `family-floor` violations are satellite lanes sitting on the 6 h default instead of their family pin, the 4 `targets` violations are the `hermes-dagger-*` satellites whose primary project is disabled, the 3 `coverage` violations are primaries missing an enabled satellite, the two `cooldown` + one `parity` violations are the `hermes-canopy` / `coding-hermes-tools` 900 s rows from §4.1 and §5, and the 32 `sync-orientation` violations are enabled `*-sync` lanes whose shell workdir carries no README (SCHED-GAP-094 — the gate is new and the provisioning of those files belongs to the sync-workdirs lane, so this number is expected to fall as those files land; it is a finding, not a reason to relax the rule).

`--board-only` (F2) is the **CI** shape and is expected green without fleet state — a CI runner has no `~/.hermes` DB. Live: `PASS — 0 violation(s) (board-only mode: fleet checks 1-7 not run — no local DB/TOML)`, exit 0. It is wired as a step in `.github/workflows/ci.yml`.

**Honest scope limit, as required:** this checker's class set is *actively growing* and being fixture-tested. Its newest classes (satellite `coverage` + `family-floor`, the board vocabulary and duplicate classes, the retired-driver pin) are covered by `tests/test_check_fleet_invariants_*.py`, and the board-vocabulary and retired-driver sets are pinned equal to their Go sources by tests — but **do not read "exit 0" as "the fleet is correct."** Read it as "the assertions this script currently makes all hold." A class it does not yet implement is silently absent from its verdict; the row that owns that expansion is `SCHED-GAP-147`, still open.

### 6.2 `fleet-cooldown-policy.py --verify` — the operator-pin tripwire

The second gate is narrower and sharper: for every project in `ELEVATED_PINS`, the `fleet.toml` pin and the live DB row must **both** carry the canonical value. Exit 0 = parity; exit 1 prints `MISMATCH <project> <field>: db=<v> toml=<v>` per disagreement. It is read-only — no PUT, no regen, no evaluation loop.

```bash
# F4 — the tripwire
python3 ~/.hermes/scripts/fleet-cooldown-policy.py --verify
```

Live result (2026-09-20, exit 1):

```
MISMATCH hermes-canopy cooldown_s: db=900 toml=21600 (canonical pin=21600)

1 pin mismatch(es) — drift detected
```

**Never run this script without `--verify`.** Without the flag it takes the evaluation path; with `--apply` it issues **live fleet-wide `PUT`s and rewrites `~/.hermes/fleet.toml`**. `--verify` is the only safe invocation, and it is the only one used in this page.

**Its scope is deliberately partial, and you should know that before trusting a green:** `--verify` covers the `ELEVATED_PINS` set only. A project outside that set is not checked here — the file is regenerated from the live value for such rows so they cannot drift *out* of the file, but nothing asserts they are *right*. That is what §6.1's `cooldown`/`family-floor` classes are for. **Use both gates; neither alone is sufficient.** As with everything else on this page, a green here means the assertion this script makes, not that the fleet is healthy.

Also note: `--verify` also prints `WARN … adaptive_cooldown: db=1` lines for armed rows in tasks-admission namespaces. Those are WARNs, not mismatches, and they do not change the exit code — the arming tripwire owns that check.

### 6.3 How to read a failure, and what to do

| Symptom | Meaning | Do |
|---|---|---|
| `VIOLATION caps <ns>: max_concurrent=… expected 1` | A satellite family can hold more than one global slot, so it can starve the foremen | `PUT /api/v1/namespaces/{ns}` `{"max_concurrent": 1}`, mirror, re-run F1 |
| `VIOLATION admission <ns>: mode=…` | A satellite namespace is `tasks`, or the foremen namespace is not | See §3.2; for a satellite, `cooldown` is the only correct answer |
| `VIOLATION cooldown <p>: cooldown_s=… below the 21600s (6h) floor` | A lane is running faster than the law, or a wake value never got reverted | Decide intent; if it is intentional, it needs a dated `ELEVATED_PINS` entry (§4.2), otherwise raise it |
| `VIOLATION family-floor <p>` | A satellite is off its family cadence — usually the REDUCE rule pulled it to the 6 h default | Restore **both** `cooldown_s` and `cooldown_floor_s` to the family value |
| `VIOLATION parity <p> <field>: db=… toml=…` | **The durability law is broken right now** | Fix the DB (API) *and* the file through its writer, then re-run F1 **and** F4 |
| `MISMATCH <p> cooldown_s: db=… toml=…` | Same, scoped to an operator pin | Same fix; re-run F4 |
| `VIOLATION targets <sat>: target is DISABLED` | A satellite is pointed at a project that is not enabled | Either re-enable the primary or disable the satellite |
| `WARN <p> adaptive_cooldown: db=1 … tasks-admission namespace` | An armed row in a `tasks` namespace — arming cannot pace a lane that spawns from board state | Disarm; do not change the expected-parity rule |

Triage order when a lane's cadence looks wrong: run F1 to see whether it is a named violation class at all; then §8's command list to compare all four sources (API, DB row, file block, code constant) for that one lane; then `docs/troubleshooting-scheduling-errors.md` for symptom-level diagnosis.

**This page is only as good as the commands in it.** If a command here no longer reproduces its stated output, the page has drifted — fix the page in the same commit that changed the behaviour. A config claim with no command behind it is not documentation, it is folklore, and folklore is exactly what this page exists to replace.

---

## 7. Findings filed, not fixed

This page is a documentation change; live fleet config is out of scope. These are live, reproducible, and each is named with its command. None is fixed by writing this page.

| # | Finding | Command that shows it | Consequence |
|---|---|---|---|
| 1 | **`hermes-canopy` cooldown drift.** DB `900` vs `fleet.toml` `21600`, while `ELEVATED_PINS` says the canonical pin is `21600`. Both gates name it independently (`parity` violation + `MISMATCH`). | S1 / S2 · F1 · F4 | The lane runs 15 minutes instead of 6 hours until a restart re-pins it — i.e. the drift is also a *latent* behaviour change at the next boot. |
| 2 | **`coding-hermes-tools` below the floor.** DB `cooldown_s = 900` against a 6 h law, and the project has **no `fleet.toml` block at all** — so the `parity` class structurally cannot see it; only the `cooldown` law class catches it. | D3 · F1 | Running 24× faster than the law, caught by exactly one of the two checks. Worth noting that "the two gates" are not redundant here. |
| 3 | **51 `family-floor` violations.** `-qa` and `-sync` lanes sit on the 21600 default instead of the 43200 family pin, so their cadence cannot be read as a signal. | F1 · D1 | A satellite that should tick twice a day ticks up to four times. |
| 4 | **The policy writer and the family pin disagree — the mirrored constant is absent.** `SATELLITE_FAMILY_PINS` exists in the repo gate and is checked for equality against the writer's copy, but the writer's working copy currently has **no** such constant (0 occurrences), while the repo test's own skip message records that a dirty working tree is the cause (SCHED-GAP-181 sibling-tick churn). The test therefore **skips** rather than failing. | `grep -c SATELLITE_FAMILY_PINS ~/.hermes/scripts/fleet-cooldown-policy.py` → `0` · `python3 -m pytest tests/test_check_fleet_invariants_coverage_and_family_floor.py -q` → `4 passed, 1 skipped` | The gate still fires on real drift (finding 3 proves the check works), but the writer-side mirror cannot be verified while the constant is missing — the cross-check is dormant. |
| 5 | **Four `targets` violations:** the `hermes-dagger-{pm,qa,sync,dogfood}` satellites are enabled while `hermes-dagger` is disabled. | `sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT name, enabled FROM projects WHERE name LIKE 'hermes-dagger%' ORDER BY name;"` → `hermes-dagger\|0`, four satellites `\|1` · F1 | Those lanes burn clean-machine batteries against an unenabled primary. |
| 6 | **Two writers, one header.** `~/.hermes/fleet.toml`'s header names `fleet-cooldown-policy.py --apply` as its generator, while `~/.hermes/scripts/fleet-sync.py` documents itself as that path's superseding decision-free DB→file mirror. The file's current content proves the mirror did **not** write it (S2 vs S3), so an operator reading the header and an operator running the mirror would take different actions. | S2 / S3 / S4 · `head -4 ~/.hermes/fleet.toml` | Ambiguity about which writer governs the restart pin — precisely the class of confusion SCHED-GAP-121 came from. |

---

## 8. Command index (the honesty sweep)

Every command in this page, with what it establishes. All were run on 2026-09-20 against the live fleet; exit codes are in the change report for this page. Nothing on this page is a claim without a command here.

| ID | Command | Establishes |
|---|---|---|
| A1 | `systemctl --user show -p ExecStart coding-hermes-scheduler.service \| tr ';' '\n' \| grep -o -- '--max-concurrent [0-9]*'` | The global cap the running unit was started with |
| A2 | `curl -s 127.0.0.1:9090/api/v1/config \| jq .max_concurrent` | The global cap the process resolved |
| A3 | `grep -n 'flag.Int("max-concurrent"' cmd/schedulerd/main.go` | The code default |
| A4 | `grep -rn 'config.LoadConfig' --include=*.go . \| grep -v _test` | That the `SCHEDULER_MAX_CONCURRENT` env layer has no non-test caller |
| B1 | `curl -s 127.0.0.1:9090/api/v1/namespaces \| jq -r '.namespaces[] \| "\(.id)\t\(.max_concurrent)\t\(.admission_mode)\t\(.load_gate)"'` | Live namespace caps and modes |
| B2 | `sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT id, max_concurrent, admission_mode, COALESCE(load_gate,'') FROM namespaces ORDER BY id;"` | The DB rows behind B1 |
| B3 | `grep -A6 '^id = "coding-hermes"' ~/.hermes/fleet.toml \| head -7` | The restart pin for the foremen namespace |
| B4 | `sqlite3 -readonly … "SELECT count(*) FROM namespaces WHERE max_concurrent=1;"` and `… "SELECT 8 + (SELECT count(*) FROM namespaces WHERE max_concurrent=1 AND id <> 'coding-hermes');"` | How many namespaces are at cap 1, and the sum of caps vs the global 10 |
| B5 | `grep -n 'SATELLITE_NS = ' ops/check-fleet-invariants.py` | The six-namespace satellite list the caps are asserted on |
| C1 | `readlink -f <satellite-workdir>/.coding-hermes/board` (and the primary's) | The symlink that makes satellites time-based |
| C2 | `grep -n 'admission_mode TEXT\|board_ownership TEXT' internal/database/migrations.go` | Schema defaults and CHECK constraints |
| C3 | `grep -n 'func laneOwnsBoard' -A 8 internal/scheduler/admission_mode.go` | The ownership rule and its fail-closed branches |
| C4 | `sqlite3 -readonly … "SELECT COALESCE(NULLIF(admission_mode,''),'(inherit)'), COALESCE(NULLIF(board_ownership,''),'(auto)'), count(*) FROM projects GROUP BY 1,2;"` | Effective per-project mode/ownership |
| C5 | `grep -n '22 -sync' -A 2 internal/scheduler/admission_mode.go` | The live measurement that motivated the ownership rule |
| D1 | `grep -n 'COOLDOWN_FLOOR = \|COOLDOWN_TIERS = \|SATELLITE_FAMILY_PINS = ' ops/check-fleet-invariants.py` | The 6 h floor and both tier tables |
| D2 | `sqlite3 -readonly … "SELECT cooldown_s, count(*) FROM projects WHERE enabled=1 GROUP BY cooldown_s ORDER BY cooldown_s;"` | The live cadence histogram |
| D3 | `sqlite3 -readonly … "SELECT name, cooldown_s, cooldown_floor_s FROM projects WHERE enabled=1 AND cooldown_s < 21600;"` | Every enabled sub-floor exception |
| D4 | `grep -n 'ELEVATED_PINS = {' -A 10 ~/.hermes/scripts/fleet-cooldown-policy.py` | The anti-snap whitelist with its dated rulings |
| D5 | `grep -n '3600s fast (1h) / 21600s default (6h) / 43200s idle (12h)' …` and `grep -n 'Any fleet.toml pin outside the canonical policy set' -A 3 …` | The canonical policy set, and that a non-canonical pin is honoured |
| E1 | `grep -n 'AdaptiveCeilingFloorMultiplier = \|DefaultAdaptiveCooldownThreshold = ' internal/database/models.go` | The escalator's two constants |
| E2 | `sqlite3 -readonly … "SELECT count(*) FROM projects WHERE enabled=1 AND adaptive_cooldown=1;"` | How many lanes the escalator can affect |
| E3 | `sqlite3 -readonly … "SELECT cooldown_ceiling_s, count(*) FROM projects WHERE enabled=1 GROUP BY 1 ORDER BY 1;"` | That ceilings are 8 × floors |
| E4 | `curl -s 127.0.0.1:9090/api/v1/projects/hermes-canopy \| jq '.project \| {cooldown_s, cooldown_floor_s, cooldown_ceiling_s, adaptive_cooldown, no_progress_threshold}'` | One lane's full cooldown state |
| E5 | `grep -n 'adaptiveCooldownFactor = 2' internal/scheduler/adaptive_cooldown.go` | The escalator's multiplier |
| E6 | `sqlite3 -readonly … "SELECT count(*) FROM projects WHERE enabled=1;"` | The enabled-lane denominator |
| F1 | `python3 ops/check-fleet-invariants.py` | The live-config gate and its violation classes |
| F2 | `python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only` | The CI shape (no fleet state needed) |
| F3 | `python3 ops/check-fleet-invariants.py --json \| jq '{ok, n: (.violations\|length), checks}'` | Machine-readable verdict + classes actually run |
| F4 | `python3 ~/.hermes/scripts/fleet-cooldown-policy.py --verify` | Operator-pin parity across both stores |
| G1 | `grep -n 'boardWakeDebounce = \|boardWakePollInterval = ' internal/scheduler/board_wake.go` | The wake's poll cadence and debounce |
| G2 | `grep -n 'pendingBoostUrgency = \|bumpBoostUrgency = \|pendingBoostMaxCount = ' internal/scheduler/board_awareness.go` | The ordering tiers (not admission knobs) |
| G3 | `curl -s 127.0.0.1:9090/api/v1/config \| jq '{tasks_pacing, load_gate_threshold}'` | The pacing floor and load gate the wake does not bypass |
| S1 | `curl -s 127.0.0.1:9090/api/v1/projects/hermes-canopy \| jq '.project.cooldown_s'` | The DB value of a pinned project |
| S2 | `grep -A6 'name = "hermes-canopy"' ~/.hermes/fleet.toml \| grep cooldown_s` | The file value for the same project |
| S3 | `python3 ~/.hermes/scripts/fleet-sync.py 2>/dev/null \| grep -A6 'name = "hermes-canopy"' \| grep cooldown_s` | What the decision-free mirror would write |
| S4 | `python3 ~/.hermes/scripts/fleet-sync.py 2>&1 >/dev/null \| tail -1` | Whether the mirror was given `--write` |
| G4 | `grep -n 'starvationBoostUrgency = 1e12' internal/scheduler/fairness.go` | The starvation tier value |
| T1 | `curl -s 127.0.0.1:9090/api/v1/config \| jq .tasks_pacing` | The tasks post-tick pacing floor |
| T2 | `curl -s 127.0.0.1:9090/api/v1/config \| jq '{weight_budget, budget_source}'` | The weight budget and where its value came from |
| T3 | `uptime` | The live 1-minute loadavg the gate samples |
| T4 | `curl -s 127.0.0.1:9090/api/v1/namespaces \| jq -r '.namespaces[] \| select(.load_gate != "") \| .id'` | The only namespace opted out of the load gate |
| T5 | `grep -A 4 '^blackout_windows' ~/.hermes/fleet.toml` | The peak-pricing slowdown windows |
| P1 | `grep -c SATELLITE_FAMILY_PINS ~/.hermes/scripts/fleet-cooldown-policy.py` | Whether the writer still carries the family-pin mirror (finding 4) |
| P2 | `python3 -m pytest tests/test_check_fleet_invariants_coverage_and_family_floor.py -q` | Whether the family-floor/coverage classes are fixture-tested (and whether the writer mirror is skipped) |
| P3 | `sqlite3 -readonly ~/.hermes/coding-hermes/scheduler.db "SELECT name, enabled FROM projects WHERE name LIKE 'hermes-dagger%' ORDER BY name;"` | The disabled primary behind the four `targets` violations (finding 5) |
| P4 | `head -4 ~/.hermes/fleet.toml` | Which writer the restart pin's header names (finding 6) |

**Requirements, stated plainly.** B1/B2/B3/C1/C4/D2/D3/E2/E3/E4/F1/F3/F4/S1–S4 need the live fleet: the daemon on `127.0.0.1:9090`, `~/.hermes/coding-hermes/scheduler.db`, and `~/.hermes/fleet.toml`. A/I/…/C2/C3/D1/E1/G1/G2 read the **repo only** and work in any checkout; F2 is the one fleet gate that runs without fleet state, which is why it is the CI shape. F4 requires `~/.hermes/scripts/fleet-cooldown-policy.py`, which lives outside this repo.

---

## 9. See also

- `docs/api.md` — the REST surface, including the namespace and project field tables this page's knobs are set through.
- `docs/troubleshooting-scheduling-errors.md` — symptom → meaning → command → fix, for when a cadence looks wrong.
- `docs/runbook-drain-restart.md` — drain and restart procedure; every flag change on this page needs it.
- `docs/integration.md` — the authority model in integration terms.
- `docs/adr/002-board-ownership-gates-tasks-waiver.md` — why the board-ownership precondition is a derived filesystem rule and not a project list.
- `AGENTS.md` — the cooldown authority model and the design rulings this page summarises (the canonical narrative; this page is the operator-facing view).
- In the operator home, outside this repo: `~/.hermes/scripts/fleet-cooldown-policy.py` (the pin writer) and `~/.hermes/fleet.toml` (the restart pin).
