# ops — fleet-side operational scripts

## Invariant checker + fixture battery (SCHED-GAP-147)

`check-fleet-invariants.py` is the regression gate for the fleet's live
CONFIG shape (caps, admission modes, cooldown law, retired drivers, store
parity, board health). Config has no unit test of its own — this script is
the thing that fails when it drifts back.

The checker is provably-incapable of lying because every check class carries
a FIXTURE in `tests/` with two arms: a seeded violation must exit 1 naming
the exact offending subject, and the conforming fleet must exit 0. A class
whose fixture only proves the "fire" arm (or worse, neither) is a gate that
lies — the exact failure this fleet keeps re-learning.

### Run it

```
bash scripts/check-invariants.sh          # the whole battery, one command
```

The battery runs (1) the checker in `--board-only` mode against this
checkout's board (the CI shape) and (2) the full pytest fixture battery,
exiting non-zero if either fails. Run it after every scheduler deploy and
from the daily report. CI runs the same script in the build job — it is
environment-independent by construction (temp DBs/TOMLs/boards; a GitHub
runner has no `~/.hermes/coding-hermes/scheduler.db`).

Operator variants of the checker itself:

```
python3 ops/check-fleet-invariants.py                                   # live fleet (DB + ~/.hermes/fleet.toml)
python3 ops/check-fleet-invariants.py --global-cap 10                   # + argv↔TOML global-cap parity
python3 ops/check-fleet-invariants.py --json                            # machine-readable
python3 ops/check-fleet-invariants.py --board .coding-hermes/board/tasks.jsonl --board-only   # CI shape
```

### Class → fixture table

| class | what it asserts | fixture test | status |
|---|---|---|---|
| `caps` | foreman namespace cap = 8; every satellite namespace cap = 1; a missing satellite namespace is itself a violation | `tests/test_check_fleet_invariants_caps_and_admission.py` | covered (both arms) |
| `admission` | every namespace carries `admission_mode`; `tasks` ONLY on `coding-hermes`; satellites on `cooldown` | `tests/test_check_fleet_invariants_caps_and_admission.py` | covered (both arms) |
| `cooldown` | no enabled lane below the 21600s (6h) floor except documented tiers (qa-audit 86400, release-engineer 604800); tier drift fires; disabled lanes exempt | `tests/test_check_fleet_invariants_cooldown_executors_workdirs.py` | covered (both arms) |
| `executors` | no enabled lane `command`/`prompt` drives a retired driver (5-name tuple, mirrored by `internal/api/retired_drivers.go`); explicit `RETIRED` / `do NOT run` marker exempt | `tests/test_check_fleet_invariants_cooldown_executors_workdirs.py` + pre-existing `tests/test_check_fleet_invariants_retired_drivers.py` | covered (both arms) |
| `workdirs` | every enabled lane's workdir exists | `tests/test_check_fleet_invariants_cooldown_executors_workdirs.py` | covered (both arms) |
| `adaptive` | no enabled satellite-shaped lane (`<name>-qa/-pm/-dogfood/-sync`) has adaptive cooldown armed | `tests/test_check_fleet_invariants_adaptive_boards_coverage.py` | covered (both arms) |
| `boards` | an enabled satellite whose workdir lacks the `.coding-hermes/board` link while its target project exists fires | `tests/test_check_fleet_invariants_adaptive_boards_coverage.py` | covered (both arms) |
| `coverage` | every coding-hermes primary (namespace-discriminated) has an enabled qa/pm/sync/dogfood satellite; one violation per missing suffix; disabled = missing | `tests/test_check_fleet_invariants_adaptive_boards_coverage.py` + pre-existing `tests/test_check_fleet_invariants_coverage_and_family_floor.py` | covered (both arms) |
| `family-floor` | every enabled satellite sits on its family pin (`cooldown_s` AND `cooldown_floor_s`; qa 43200 / pm 86400 / sync 43200 / dogfood 259200) — either field drifting fires | `tests/test_check_fleet_invariants_family_floor_and_parity.py` + pre-existing coverage/family-floor module | covered (both arms) |
| `targets` | a satellite's target project exists AND is enabled; a dead/absent target with no recent activity fires; the active external-lane exemption is proven via a REAL seeded completed tick (INFO, exit 0) | `tests/test_check_fleet_invariants_targets.py` | covered (both arms) |
| `parity` | DB↔fleet.toml agreement on `cooldown_s`/`cooldown_floor_s`/`cooldown_ceiling_s`/`board_ownership` per project and `admission_mode`/`max_concurrent` per namespace; row absent from the toml is skipped BY DESIGN (partial mirror) | `tests/test_check_fleet_invariants_family_floor_and_parity.py` | covered (both arms) |
| `parity` (global cap) | the GLOBAL `--max-concurrent` has no DB column — it is ARGV↔TOML parity: pass the daemon's real value as `--global-cap N` and a stale `[scheduler] max_concurrent` pin fires. **Skipped when the flag is omitted** (the checker cannot see the daemon's argv — this is a documented skip, not coverage) | `tests/test_check_fleet_invariants_global_cap_parity.py` | covered (both arms; skip shape pinned) |
| `board-vocab` | every board row carries a dispatchable status (`pending`/`in_progress`/`complete`/`duplicate`) | pre-existing `tests/test_check_fleet_invariants_board_vocab.py` | covered (pre-existing) |
| `board-legacy-status` | legacy closed spellings (`done`/`completed`/`closed`) get their own violation so the PM cycle sweeps them | `tests/test_check_fleet_invariants_board_legacy_status.py` | covered (both arms) |
| `board-content-dup` | two open rows with identical non-volatile content fire as one per-group violation; all-closed groups exempt; volatile-field-only diffs still collide | pre-existing `tests/test_check_fleet_invariants_board_dup_content.py` | covered (pre-existing) |
| `sync-orientation` | every enabled `*-sync` lane's workdir carries a non-empty `README.md` naming its target DuckBrain namespace (a `namespace` line carrying the lane's base as a whole hyphen-delimited word, or a >=2-token hyphen run of it) AND a findable consumption contract (the companion `<base>-sync-data` skill under the skills root, or an in-README pointer: `/sync/` marker, `/api/` route, skill name). One violation per lane with every unmet fact in the detail; a workdir ABSENT is check 5's fact (no double-report); disabled lanes exempt; the skill axis SKIPS SILENTLY when the skills root is absent (`--skills-root`, the CI shape) while the README facts still assert | `tests/test_check_fleet_invariants_sync_orientation.py` | covered (seeded / conforming / exempt / skip arms + per-fact independence) |

The row's PASSING-FLEET requirement lives in
`test_passing_fleet_full_checker_exits_zero` (a full conforming fleet exits 0
against the complete checker) with its negative control
`test_passing_fleet_toml_drift_flips_it_red` (the same fleet + one stale pin
exits 1), both in `tests/test_check_fleet_invariants_family_floor_and_parity.py`.

### Known skip shapes (documented, NOT hidden coverage)

* **Global cap** — no DB column to assert a constant against; parity-only via
  `--global-cap`, skipped when the operator omits it (deploy runbooks should
  pass the daemon's actual flag value).
* **`board_ownership` parity on an older DB** — when the projects table lacks
  the column, the field is skipped (`have_ownership=False`). The skip shape
  is pinned by
  `test_parity_board_ownership_absent_column_is_skipped_not_passed` so it can
  never masquerade as a passing parity assertion.
* **Board checks** — silently skipped when no board file is given/found (the
  checker's documented behavior for test rigs); the battery always passes an
  explicit board, so this skip cannot hide anything in CI.
* **`sync-orientation` skill axis** — silently skipped when the skills root
  (`--skills-root`) does not exist, because a CI runner carries no
  `~/.hermes/skills`. The README facts (non-empty file, namespace named) are
  workdir-local and STILL assert, so the class is never vacuous in CI; only the
  companion-skill half of the contract goes unchecked there. A lane whose
  workdir is absent is check 5's finding — the class stays silent so one fact
  never produces two lines.

### Adding an invariant

**Adding an invariant without a fixture is itself a defect.** The checklist:

1. Add the check to `ops/check-fleet-invariants.py` with a named class in
   `CHECK_CLASSES` (or reuse an existing class).
2. Add a fixture module in `tests/` following the existing pattern: import
   the checker by path (`importlib.util.spec_from_file_location`), seed a
   temp SQLite DB / TOML / JSONL board with the violation, run
   `gate.main([...])` in-process, and assert BOTH the seeded arm (exit 1 +
   the exact `VIOLATION <class> <subject>` line) and the conforming arm
   (exit 0). Guard the seeded arm with an "no other class fired" assertion so
   a cross-firing fixture fails loudly instead of lying green.
3. Run `bash scripts/check-invariants.sh` — both arms green.
4. Add a row to the class table above. A class missing from the table is a
   gap in the gate.
5. If the check genuinely cannot run without the live fleet DB/TOML, say so
   IN THE TABLE with the reason and the skip shape — do not ship a fixture
   that passes because the check never ran.
