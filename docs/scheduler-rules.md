# Scheduler Rules — the fleet's operating law, in one place

Every rule below was ruled by the owner (the owner) and is stated with **when it was ruled**,
**where it is enforced**, and **its current status**. When a rule and its enforcement
disagree, the enforcement is the bug — fix the code, never re-litigate the rule.

Status vocabulary:
- **enforced** — live in code/config today and verified.
- **filed** — recorded as a scheduler board row (named) because the enforcement is a
  code change that has not landed yet.
- **tripwire** — detected automatically (audit script / cron), not yet prevented.

---

## 1. Lane classes: foreman vs satellite

**R1.1 — A foreman is `admission_mode=tasks`; every other lane is `cooldown`.**
*Ruled 2026-10-01.* Board-driven by design: a foreman is *supposed* to run faster than
its cooldown pin when its board has work. A satellite is paced by its cooldown alone.
Enforced (config). Verified: 36 foremen on `tasks`, 344 satellites on `cooldown`, 0 mismatches.

**R1.2 — A lane that OWNS satellites is a foreman, even when it carries a `parent`.**
*Ruled 2026-10-01.* `parent` is org nesting, not class. logsey/pulse/lore/digest under
`h3`, and `release-engineer` under `coding-hermes-scheduler`, are foremen.
Enforced (class derivation).

**R1.3 — All foremen live in ONE namespace, and it is a `tasks` namespace.**
*Ruled 2026-10-02 ("ALL FOREMAN NEED TO BE IN SAME NAMESPACE").* Today that namespace is
`coding-hermes`. Satellites live in their role namespaces: `qa`, `pm`, `dogfood`,
`duckbrain-sync`, `releases`, `review`, `docs`, `perf`.
Enforced (config). Verified: 0 foremen outside a tasks-mode namespace.

**R1.4 — A lane-level `admission_mode` beats both the lane's cooldown and its namespace,
so it must never contradict the class.** *Ruled 2026-10-01.* A stray lane-level `tasks`
on a satellite makes it board-driven forever (release-engineer reached 250 ticks on a
168h pin this way); the mirror — a foreman left on `cooldown` — makes a full board wait
out a pin. Enforcement belongs at the **boundary** (config load + `PUT`), never by hand.
**Filed: SCHED-GAP-1696.** Interim: `fleet_runrate_audit.py` reports violations (3b/3c)
and `fleet-cooldown-policy.py` names them without writing.

**R1.5 — The identity is IN THE NAME: a foreman lane is `<project>-foreman`.**
*Ruled 2026-10-02.* Satellite lanes are `<project>-<role>`. This is why every tick id,
Telegram subject, board header and log line says what it is without any display
heuristic. Enforced (64 real project lanes renamed; parents, tick history and
`fleet.toml` carried along).

**R1.6 — The role suffixes are exactly:** `-qa -pm -sync -dogfood -perf -releng
-review -docs -readme`. A lane carrying one is a satellite. No other suffix confers
satellite status.

---

## 2. Wake

**R2.1 — `board_wake` applies to TASK-MODE lanes only.**
*Ruled 2026-10-01.* A board write turns the project back on so it fires immediately
instead of waiting out its pin — that is why writing a task to a foreman on a 2-week
cooldown starts work now. **Cooldown satellites are never woken.**
**Filed: SCHED-GAP-1695.** Measured defect at ruling time: 580 of 1,085 wake ticks went
to cooldown lanes.

**R2.2 — A task-mode lane with an EMPTY board falls back to cooldown speed.**
Exception: self-generating foremen (`never-done` and peers) exist to find their own work
and keep flowing on an empty board. *Ruled 2026-10-01.*

**R2.3 — Over-waking is fixed in TARGET ENUMERATION, not admission.**
*Ruled 2026-10-01.* The watcher must not be handed a satellite in the first place.
"Wake fewer lanes" was the wrong frame; it is class, not volume. **Filed: SCHED-GAP-1695.**

**R2.4 — A tick that runs early must stamp WHY it ran early.**
`admit_reason` / `nudge_source` must say `board_wake` (board had work) rather than a
generic `ok`, so "ran early because the board had work" (correct) is distinguishable
from "ran early on an empty board" (a defect). *Ruled 2026-10-01.* **Filed: SCHED-GAP-1695.**

---

## 3. Slots and concurrency

**R3.1 — The global budget is 20 concurrent ticks of any kind.** (`--max-concurrent 20`)

**R3.2 — Foremen: `max_concurrent = 16`, `reserved = 8`.**
*Ruled 2026-10-02.* The reserved floor is **never scaled** and satellites cannot consume
it, so 8 slots are always available to foremen; foremen may reach 16 when satellites are
idle. Enforced (config + `fleet.toml`).

**R3.3 — Every other namespace: `max_concurrent = 1`, `reserved = 0`.**
*Ruled 2026-10-02 ("every other satellite is at 1 and only 1").* One lane at a time per
satellite namespace, no floor. Enforced (config). Verified: 0 namespaces deviate.

**R3.4 — Foremen always fill a free slot; satellites take only the remainder.**
The intent is that satellites can never occupy the capacity foremen need. Today the
allocator's integer flooring computes satellite shares as **0** and leaves the budget
under-allocated (9 of 20 in the measured case), so it cannot yet express this rule.
**Filed: SCHED-GAP-1697.**

**R3.5 — The load gate is the second throttle.** Effective concurrency is
`min(slots, load-gate)`. A busy satellite fleet drives 1-minute load past the gate
threshold and defers foreman spawns too — which is why capping satellites matters as much
as reserving for foremen. *Ruled 2026-10-02.*

---

## 4. Satellite stability

**R4.1 — Satellites are metronomes: fixed cadence, nothing may adjust their speed.**
*Ruled 2026-10-02.* No adaptive cooldown, no floor shorter than the cadence, no ceiling,
never woken. Verified: 0 satellites with `adaptive_cooldown` set, 0 with a floor
differing from their cadence. (The floor's only reader is `adaptiveCooldown()`, which
returns early when adaptive is off — the columns were dead, and are now uniform too.)

**R4.2 — The family cadence matrix** (one canonical speed per satellite role):
`-qa` 6h · `-sync` 6h · `-pm` 24h · `-releng` 24h · `-dogfood` 72h ·
`-perf` / `-review` / `-docs` / `-readme` weekly.

**R4.3 — Pause ≠ delete.** A paused lane keeps its row, board, prompt and history, and
carries a `disabled_reason` saying who paused it and that nobody may silently re-enable
it. Verified in use (the `python-audit*` family, 45 lanes, paused 2026-10-02).

---

## 5. Ticks and timeouts

**R5.1 — A tick ENDS when its dispatchable work is done.** An async judge or battery is
handed off; the verdict is collected by a later tick or a reaper — never by in-session
`sleep`-polling. **Filed: SCHED-GAP-1698.** Measured: 22 timeouts in 48h held **114.5
slot-hours**, containing 320 `sleep N` polls; a poll is a stream event, so the 30-minute
idle deadline cannot see it (largest silence gap inside one 3h tick: 7 minutes).

**R5.2 — A judge verdict of INCOMPLETE is terminal for that tick.** File it as evidence
and stop; never blind re-dispatch. *Ruled 2026-10-02* after a tick ran the same task's
judge twice and held its slot for 3h. **Filed: SCHED-GAP-1698.**

**R5.3 — A wall-killed tick books `status=timeout`**, never
`failure_reason=gateway_transport` (the deadline is not a transport failure).
**Filed: SCHED-GAP-1693.**

**R5.4 — Close-out INCLUDES the push.** A verified commit on local disk is not done
work. Foreman Step 8 must push and confirm. **Filed: SCHED-GAP-1694**; net =
`fleet-strand-push.py`, cron `a9a80963d48e` (:00/:30, silent when clean).

**R5.5 — The scheduler owns the end-of-tick DuckBrain hook**, fired in-session; the
DuckBrain summary sync may slow down once every session writes its own record.
**Filed: SCHED-GAP-1688 / 1689.**

**R5.6 — The package test suite must be verified with `-count=1`.** `go test` caches a
green run, so a guard PASS can mean "the suite was never executed". Measured 2026-10-02:
both `internal/scheduler` and `internal/api` time out under `-count=1` while the guard
reported PASS from cache. **Filed: SCHED-GAP-1700.**

---

## 6. Paths and layout

**R6.1 — A project's directory lives under its GitHub org: `~/<org>/<repo>`.**
*Ruled 2026-10-02.* guard/mischief/trouble belong to the `trouble-agent` org, so they
live in `~/trouble-agent/`. 64 project directories aligned; the move also updates the DB
workdir, satellites' `repo_url` and prompts, board symlinks, git worktrees and
`fleet.toml`.

**R6.2 — A satellite shares its foreman's board by symlink** from a per-role stand-in
workdir (qa/pm/dogfood/review/docs/readme/perf/releng-lane/sync-workdirs). It never
writes the project's repo directly.

**R6.3 — Org == repo is not nested.** `terminal-jail/terminal-jail` already carries the
org name; it is not moved into a directory of the same name.

---

## 7. Operating the config layer

**R7.1 — `fleet-cooldown-policy.py` mirrors the DB into `fleet.toml`; it does not get to
override live config.** It has a history of overriding lanes, so the admission-law check
inside it is **report-only** (names a violation, writes nothing). Enforcement lives at
the scheduler boundary (R1.4) and detection in the audit (3b/3c).

**R7.2 — `fleet.toml` is the durable layer.** A config change that is not mirrored there
is lost at the next restart. Operator elevated pins are honoured: the regen reads the
existing pins and hard-skips them.

**R7.3 — Rules are enforced structurally, not by hand-fix or display heuristic.**
*Ruled 2026-10-02.* If a value must always be true, the boundary enforces it (loader,
`PUT`, test). Identity that a reader needs is carried in the data (the name), not
computed at display time.

---

## 8. Where each rule is checked

| check | what it covers |
| --- | --- |
| `~/.hermes/scripts/fleet_runrate_audit.py` | class vs mode (3b/3c), satellites past their cooldown, wake defects, foremen missing `tasks`. Cron `871baf7a3e7d`, Mondays 09:00, exits non-zero on any finding. |
| `~/.hermes/scripts/fleet-cooldown-policy.py --dry-run` | cadence vs the family matrix, operator pins, admission-law violations (report-only). |
| `~/.hermes/scripts/stabilize_satellites.py` | satellite floors/ceilings/adaptive (R4.1). |
| `~/.hermes/scripts/pause_python_audit.py` | the pause-with-reason pattern (R4.3). |
| board rows SCHED-GAP-1693/1694/1695/1696/1697/1698/1700 | the rules whose enforcement is still a code change. |

## 9. The one-line summary

> Foremen are board-driven, named `<project>-foreman`, all in one tasks namespace, with 8
> reserved slots and a cap of 16. Satellites are metronomes — one per namespace, fixed
> cadence, never woken, no floors. The global budget is 20. A tick ends when its work is
> done and always pushes. Nothing adjusts a satellite's speed, and no rule is enforced by
> a display heuristic.
