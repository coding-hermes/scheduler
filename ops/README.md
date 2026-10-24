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

The battery always runs three stages: the invariant checker, the lint-guard
regression battery, and the full pytest fixture battery. After a scheduler
deploy, the first stage resolves `~/.hermes/coding-hermes/scheduler.db` and
`~/.hermes/fleet.toml`; when both are readable it runs the checker in live-fleet
mode and the summary reports the live violation count. This is the operator
verdict — a live violation makes the battery exit 1. A failing live run is
summarized as `BATTERY: FAIL (live fleet checked; live violations=791; ...)`
(the number is measured at run time).

When the DB/TOML pair is absent (the normal CI shape), stage 1 explicitly runs
`--board-only` against this checkout's board. A clean result is intentionally
reported as `BATTERY: PASS (board-only; live fleet NOT checked; fleet checks not
run: no local DB/TOML; ...)`, not as fleet conformance. This keeps CI usable
without `~/.hermes` state while making the skipped live checks visible in the
summary. The command exits non-zero if any of the three executed stages fails.
Run it after every scheduler deploy and from the daily report.

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
| `namespace-membership` | a satellite lane `<base>-<role>` must live in its ROLE's shared family namespace per the pin map (`qa`→`qa`, `pm`→`pm`, `sync`→`duckbrain-sync`, `dogfood`→`dogfood`, `releng`→`releases`, `perf`→`perf`, `review`→`review`, `readme`/`docs`→`docs`) — one line per misplaced row naming the actual and the conventional namespace (SCHED-GAP-1604: the auger family parked five satellites in a private `auger` namespace). The matcher is DERIVED from the very map it polices, so a family can be added to the pins and never left unpoliceable; lanes without a satellite suffix (foreman primaries) are not policed; DISABLED misplaced rows still fire (one resume away from live) | `tests/test_check_fleet_invariants_namespace_membership.py` | covered (seeded / null-namespace / disabled / conforming / primary-exempt / live-fleet arms). **SKIPS in `--board-only` (CI) with no line at all** — the fact lives in `projects.namespace_id`, which only the live DB carries |
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
| `board-id-slot` | an id holding TWO OR MORE OPEN rows is a recurring SLOT, not a finding — the lane re-filed each cycle under the same id without ever closing the previous row (measured 2026-09-22: 29% of open rows fleet-wide collided on id; QA-CONSENSUS-1 held 85 open rows under one id — SCHED-GAP-207). One line per id naming the open-row count; a group where every row is closed is exempt, and so is exactly ONE open row under an id with closed history (the legitimate close-then-refile cycle); status vocabulary follows the readers — legacy `done`/`completed`/`closed` are CLOSED, an off-vocabulary or missing status is OPEN, never hidden work | `tests/test_check_fleet_invariants_board_id_slot.py` | covered (seeded / refile-after-close / closed-history / distinct-id / off-vocab arms). **ASSERTS in `--board-only` (CI)** — ids come from the board file; the class skips silently only when no board resolves (checks 8/9's shape). Its per-board census rides a **`open-rows`** INFO line (`open rows=N unique open ids=M slot-inflated rows=K`) — a METER alias over every open row, deliberately NOT a `CHECK_CLASSES` entry: the class name is `board-id-slot` |
| `sync-orientation` | every enabled `*-sync` lane's workdir carries a non-empty `README.md` naming its target DuckBrain namespace (a `namespace` line carrying the lane's base as a whole hyphen-delimited word, or a >=2-token hyphen run of it) AND a findable consumption contract (the companion `<base>-sync-data` skill under the skills root, or an in-README pointer: `/sync/` marker, `/api/` route, skill name). One violation per lane with every unmet fact in the detail; a workdir ABSENT is check 5's fact (no double-report); disabled lanes exempt; the skill axis SKIPS SILENTLY when the skills root is absent (`--skills-root`, the CI shape) while the README facts still assert | `tests/test_check_fleet_invariants_sync_orientation.py` | covered (seeded / conforming / exempt / skip arms + per-fact independence) |
| `event-id-ascending` | every INTEGER id in `events.jsonl` (resolved from `--events`, else the sibling of `--board`, else the walk-up from the script) sits on the board's 19-digit epoch-nanosecond scale (`>= 10^18`) and does not descend below the highest id already in the file — the defect that planted two epoch-microsecond lines in this project's log and FAILed the live `boardctl validate` (SCHED-GAP-206). One violation per line, naming the LINE NUMBER and the BAD ID. Lines that fail to parse or carry no int id are skipped; the file's PRE-SCALE prefix (ids before its first `>= 10^18` id — this board has 979 on lines 1-999) and duplicate ids are tolerated, exactly as `boardctl validateEvents` tolerates them; a file with no 19-digit id at all is entirely off-scale and every int id in it fires. A missing events file SKIPS SILENTLY, same as checks 8/9 | `tests/test_check_fleet_invariants_event_ids.py` + Go `internal/scheduler/board_event_id_test.go` (`ValidateEventIDs`, floor pinned equal by `TestValidateEventIDs_ScaleMatchesPythonGate`) | covered (seeded / conforming / tolerant-prefix / duplicate / legacy-shape / skip arms) |
| `lane-instructions` | every ENABLED lane carries instructions: the CONJUNCTION of own `prompt` AND its namespace `default_prompt` fallback both under `PROMPT_SHORT_CHARS` (400 — above the 296-char auger-releng incident state, below the thinnest healthy own prompt 1060 and fallback 777; justified at the constant) fires one line naming the lane; a `prompt_mode=replace` lane is judged on its own prompt ALONE (it replaces the base); disabled lanes exempt; skips with a documented INFO line when the schema predates the prompt columns; rides the live-DB pass so `--board-only` (CI) never runs it | `tests/test_check_fleet_invariants_lane_instructions.py` | covered (seeded / conforming / replace / boundary / disabled / schema-skip / board-only arms) |
| `cooldown-pin-pacing` | `cooldown_pin_s` is a promise, so the OBSERVED inter-tick pacing must honour it: a cooldown-mode lane's MEDIAN gap between completed ticks must be at least HALF its effective base (`cooldown_pin_s` when set and positive, else `cooldown_s`) — under half means the lane re-ticked at least twice as fast as its own base promises (the perf-lane shape: ~25-30h gaps against a 7d floor — SCHED-GAP-1661). WINDOW is era-aware: the last 7 days, or since the lane's `updated_at` config write, because the fleet policy sync re-stamps every row and pre-era ticks measure the PREVIOUS config; backoff/blackout terms are excluded from the base so a failing lane cannot mask its pacing; fewer than 3 in-era completed ticks = no median yet. `tasks`-admission lanes are skipped as not timer-paced (the SCHED-GAP-124 pending-work waiver) and the `*-perf` lanes listed in `COOLDOWN_PIN_PACING_EXCEPTIONS` are exempt with a per-lane reason | `tests/test_check_fleet_invariants_cooldown_pin_pacing.py` | covered (seeded / no-pin / zero-pin / conforming / exact-half boundary / unmeasurable / dynamic-skip / disabled / exception / era-aware / tasks-waiver / schema-skip / board-only arms). **SKIPS in `--board-only` (CI)** — it reads the `ticks` table, so the battery never runs it. On a live DB it emits `INFO cooldown-pin-pacing schema: skipped — projects/ticks schema predates cooldown_pin_s or tick timestamps` for an old schema and a per-lane `skipped — no measurable base` INFO for a dynamic-interval (`cooldown_s=0`) lane |
| `fleet-infra-dup` | a PROJECT board carries an OPEN row whose pattern the shared-infra board already carries open as a `FLEET-INFRA-N` carrier (`classification: fleet-infra:<pattern>` / `origin: fleet-infra-classifier|fleet-infra-collapse`) — the SCHED-GAP-128 defect where one host condition became N project rows. Rows that merely MENTION the vocabulary (a repo's own stale binary, a docs row quoting a config drop) are project-local to the same classifier the router uses, so the class never demands their deletion; closed copies exempt; the infra board's own native rows exempt. Resolves the carrier side itself in live mode; in `--board-only` (CI) it SKIPS with a documented INFO unless `--infra-board` + `--fleet-boards` fixtures are passed | `tests/test_check_fleet_invariants_fleet_infra_dup.py` | covered (seeded / clean / no-carrier / skip / table-registered arms) |

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
  explicit board, so this skip cannot hide anything in CI. The SAME skip covers
  the events log (`event-id-ascending`, check 11): a board directory with no
  `events.jsonl` produces no line for the class at all.
* **`lane-instructions` on an old schema** — when the DB schema predates the
  prompt columns (`projects.prompt` / `namespaces.default_prompt`), the class
  emits the documented skip INFO line and no violation, never silently passes
  as an assertion (same shape as the `board_ownership` skip, pinned by
  `test_schema_without_prompt_columns_skips_with_info`). A DB without the
  column cannot carry the defect the class exists for, and the fixture DBs of
  every pre-existing test module mint exactly that old schema.
* **`sync-orientation` skill axis** — silently skipped when the skills root
  (`--skills-root`) does not exist, because a CI runner carries no
  `~/.hermes/skills`. The README facts (non-empty file, namespace named) are
  workdir-local and STILL assert, so the class is never vacuous in CI; only the
  companion-skill half of the contract goes unchecked there. A lane whose
  workdir is absent is check 5's finding — the class stays silent so one fact
  never produces two lines.
* **`namespace-membership` in `--board-only`** — the asserted fact is
  `projects.namespace_id`, which only the live DB carries; the board file has
  no namespace column, so the CI shape never evaluates the class and emits NO
  line for it. That silent skip is why the row above says so explicitly: the
  fixture battery seeds temp DBs and covers both arms, but the CI run alone can
  never regress this class, and a reader must not read the clean battery as
  coverage of it.
* **`cooldown-pin-pacing` in `--board-only`** — the class reads the `ticks`
  table, so the CI shape never runs it (no ticks table exists there). On a live
  DB it skips with an explicit INFO line — `skipped — projects/ticks schema
  predates cooldown_pin_s or tick timestamps` for an old fixture schema, and
  `skipped — no measurable base` per lane when `cooldown_s=0` (dynamic
  interval, not observable from ticks) — plus the per-lane `tasks`-mode waiver
  counted in its census INFO. A NULL median always carries a reason; it never
  passes silently as an assertion.

* **`fleet-infra-dup` in `--board-only`** — the class must compare two sets of
  BOARDS (project boards vs the shared-infra board), and CI carries neither.
  With `--infra-board` + `--fleet-boards` it runs anywhere (the fixture battery
  drives it that way); without them it emits a documented skip INFO. Live
  `python3 ops/check-fleet-invariants.py` resolves both sides from the DB.

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
   gap in the gate, and the table is now a TESTED surface:
   `tests/test_check_fleet_invariants_class_table.py` fails the build when
   `CHECK_CLASSES` and the table drift in EITHER direction (a class with no
   row, a row naming a class that no longer exists, a duplicate row, or a
   fixture path that does not resolve) — the same parity shape
   `internal/mcp/agents_endpoint_parity_test.go` holds for the route table.
   Flag-only variants (`parity` with `--global-cap`) are not `CHECK_CLASSES`
   entries; list them in that test's `FLAG_ONLY_ROWS`. An INFO line tagged
   with a name that is neither a class nor a listed METER alias
   (`open-rows`) fails it too, so a meter can never masquerade as a class.
5. If the check genuinely cannot run without the live fleet DB/TOML, say so
   IN THE TABLE with the reason and the skip shape — do not ship a fixture
   that passes because the check never ran. A row added from now on must also
   state what happens in `--board-only` (the CI shape) — it asserts, or it
   skips, and with which line — so no reader can mistake a CI skip for
   coverage; the three DOC-25 rows and `tests/test_check_fleet_invariants_class_table.py`
   hold that shape (`BOARD_ONLY_DOCUMENTED`).

## Shared-infra findings: classify, file once, collapse the copies (SCHED-GAP-128)

The qa/dogfood/PM lanes report on the SHARED substrate — the bunker agent
hosts, the port pool, config delivery, the gateway key. Those conditions are
fleet-wide, but each lane filed them on the board of whatever project it was
poking, so one fix became N open rows (measured 2026-10-22: 42 fleet-infra rows
across 15 project boards for TWO fixes).

Two scripts and one gate class own the whole story:

| piece | role |
|---|---|
| `ops/fleet-infra-patterns.json` | the classification table: name, `pattern` (must match the row TITLE), optional `requires` substrate co-signal (may come from the detail), and an `exclude` list for the tracking row that NAMES the class to fix it |
| `ops/fleet-infra-findings.py` | `classify` (label a finding), `file` (route it: fleet-infra → ONE `FLEET-INFRA-N` row on the shared board with a `cross_ref`, project-local → passthrough), `stats` (fleet census), `throttle` (inspect/clear the filing ledger) |
| `ops/collapse-fleet-infra-findings.py` | the migration + the per-tick convergence sweep: collapse existing copies into ONE row per pattern, archive every removed row, back up every rewritten board |
| `ops/fleet-infra-sweep.sh` | tick-safe wrapper: `--sweep` mode, one output line, ALWAYS exit 0 |
| check 13 `fleet-infra-dup` | the regression gate: an OPEN project-board copy of a pattern the shared board already carries fires |

### The discriminator that matters

`stale-binary` is also a PROJECT class (a repo whose own deployed binary
predates HEAD — `GAP-056`, `DF-HERMES-CANOPY-32`). The `requires` half is what
separates "the host the fleet stands on is stale" from "this project ships a
stale binary", and it is matched against the TITLE + DETAIL only — never the
chatty `reasoning`/`foreman_note` prose where a lane narrates the whole fleet.
A classifier without that split sweeps real project work into the fleet bucket,
which is worse than the noise it removes.

### Filing (prevention at the source)

```bash
python3 ops/fleet-infra-findings.py classify "run_battery FAIL — bunkerd port-pool exhaustion on bunker-las-03"
# {"label": "fleet-infra:port_pool", "pattern": "port_pool", "route": "infra", ...}

python3 ops/fleet-infra-findings.py file --board <project>/tasks.jsonl \
    --project consensus --title "<finding title>" --detail "<evidence>" --apply
# action=filed|observed|throttled|passthrough|unroutable
```

`observed` means an OPEN `FLEET-INFRA` row of that pattern already existed and
this project was appended to its `cross_ref.observations` — that is how "17
projects saw it" stays one row and still counts 17. The observing board gets a
`fleet_infra_diverted` EVENTS row and no task row.

The **30-minute filing throttle** (`(project, pattern)`, NUL-separated,
`$FLEET_INFRA_THROTTLE_S` to override, ledger at
`~/.hermes/coding-hermes/fleet-infra-throttle.json`) mirrors the scheduler's
lane-health throttles (`starvationThrottleWindow`,
`consecutiveFailureThrottleWindow` — both 30 min, per project, read back so the
memory survives a fresh process). Two lanes filing the same host condition
inside the window produce one row.

### Migration (already applied once, 2026-10-22)

```bash
python3 ops/collapse-fleet-infra-findings.py                # dry run (default)
python3 ops/collapse-fleet-infra-findings.py --apply        # migrate
python3 ops/collapse-fleet-infra-findings.py --apply --include-closed
```

* Removes ONLY rows the classifier calls fleet-infra; project-local rows that
  merely mention the vocabulary stay.
* One `FLEET-INFRA-N` row per pattern, `cross_ref.observations` naming every
  collapsed `(project, row id, status, priority)`; priority is the strongest
  the copies carried.
* Removed rows are archived verbatim to
  `~/.hermes/coding-hermes/fleet-infra-collapse/collapse-<UTC>.jsonl` with a
  `_collapsed` provenance block.
* Each rewritten board is backed up to
  `~/.hermes/coding-hermes/fleet-infra-backups/<project>/tasks.jsonl.bak-<UTC>`
  BEFORE the rewrite; the rewrite itself is temp-file + rename, re-emitting
  untouched lines byte-identically.
* Lost-update guard: the rewrite is refused when a board's
  `(size, mtime_ns)` moved since the classification read — a lane that appended
  mid-run is never silently dropped, and the run reports
  `SKIPPED (concurrent write)`.
* Idempotent: a second `--apply` reports `collapsed=0`.

Rollback one board:
`cp ~/.hermes/coding-hermes/fleet-infra-backups/<project>/tasks.jsonl.bak-<stamp> <repo>/.coding-hermes/board/tasks.jsonl`.

### Per-tick convergence

Rewiring every lane's filing path is the prevention; the sweep is the
convergence, and it is what makes the row's acceptance criterion hold after the
NEXT qa tick. The qa driver
(`~/.hermes/scripts/qa-scheduler-tick.sh`) calls the sweep after the pipeline
runs, resolved from `$FLEET_INFRA_SWEEP` or this checkout's
`ops/fleet-infra-sweep.sh`. Other tick drivers opt in the same way:

```bash
/path/to/coding-hermes-scheduler/ops/fleet-infra-sweep.sh || true
```
