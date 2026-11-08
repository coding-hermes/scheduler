"""SCHED-GAP-16996 regression fixtures for the ``<base>-foreman`` primary shape.

Six-plus fleet families name their coding-hermes PRIMARY lane
``<base>-foreman`` (gitreins-foreman, task-router-foreman,
coding-hermes-scheduler-foreman, …) while the family's satellites stay named
``<base>-<suffix>``. Checks 5d (``coverage``) and 6 (``targets``) used to
resolve satellites/targets against the RAW lane name, so they hunted for
``<base>-foreman-qa`` children and a ``<base>`` target row that do not exist
— SCHED-GAP-16996's live run flagged 49 such violations. The fix strips the
trailing ``-foreman`` stem (``foreman_family_base``) before resolving a
primary's satellites, and resolves a satellite's ``<base>`` target to the
``<base>-foreman`` row before declaring the target missing.

A second live shape the same row covers: a PARKED family — the primary
enabled but ALL FOUR satellites present and disabled (warpfs-foreman,
hermes-dagger-foreman, 2026-11-08). The rule reports one INFO line under the
``coverage`` class instead of four "missing or disabled" lines for lanes
that exist; the exemption is narrow (any ABSENT satellite still violates,
pinned by ``test_foreman_family_missing_satellite_still_fires`` and the
mixed-state arm).

This module drives ``ops/check-fleet-invariants.py`` in-process against a
seeded fixture DB (same rig as
``test_check_fleet_invariants_coverage_and_family_floor.py``):

  * a ``<base>-foreman`` primary with all four ``<base>-<suffix>`` satellites
    must be CLEAN — dropping the strip makes the gate hunt
    ``<base>-foreman-<suffix>`` children and turn this arm red;
  * a satellite whose ``<base>`` target exists ONLY as ``<base>-foreman``
    must resolve to that row (a DISABLED foreman row fires the DISABLED
    message, not the "neither a fleet project" one);
  * the strip must not deafen the rule: a missing satellite of a foreman
    family still fires, naming the family-shaped child, and a mixed
    absent+disabled state still violates per suffix;
  * a parked family (all four satellites disabled) is INFO, exit 0;
  * ``foreman_family_base`` strips ONLY a trailing ``-foreman`` stem —
    ``h3-sdk-go-foreman`` (a real primary) strips to ``h3-sdk-go``, while
    ``h3-sdk-go-foreman-sync`` (a satellite whose base merely CONTAINS
    ``foreman``) is untouched, exactly like the existing multi-token handling
    in ``sync_readme_declares_namespace``.

The fixture base name is chosen so every candidate path the targets check
derives for it is absent on any sane machine (premise asserted at import).

Run:  python3 -m pytest tests/test_check_fleet_invariants_foreman_primary.py -v
"""
from __future__ import annotations

import importlib.util
import io
import sqlite3
import sys
from contextlib import redirect_stdout
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[1]
GATE_PATH = REPO_ROOT / "ops" / "check-fleet-invariants.py"

SUPPORTED_COMMAND = "bash /home/kara/.hermes/scripts/scheduler-foreman-tick.sh"

# The family under test: PRIMARY carries the trailing -foreman stem, the
# satellites do NOT. BASE must not exist as a path anywhere the targets
# check looks, or the "external data target" exemption would rescue it.
BASE = "schedgap16996-fixture"
PRIMARY = f"{BASE}-foreman"
COVERAGE_SUFFIXES = ("qa", "pm", "sync", "dogfood")
SATELLITES = [f"{BASE}-{suffix}" for suffix in COVERAGE_SUFFIXES]


def _load_gate():
    """Import ops/check-fleet-invariants.py by path (hyphenated filename)."""
    spec = importlib.util.spec_from_file_location("check_fleet_invariants", GATE_PATH)
    assert spec is not None and spec.loader is not None, f"cannot load {GATE_PATH}"
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


gate = _load_gate()


# Premise of the module: the chosen base name is portable — no candidate path
# exists, so a MISSING target cannot be rescued by the disk-path exemption.
for cand in (f"/home/kara/{BASE}", f"/home/kara/{BASE.replace('-', '_')}",
             str(Path.home() / ".hermes" / BASE)):
    assert not Path(cand).exists(), f"fixture premise broken: {cand} exists on this machine"


def _make_workdir(root: Path, name: str) -> Path:
    """Workdir with the 5c ``boards`` link; ``*-sync`` lanes also get the
    check-10 orientation facts (README naming the namespace + contract
    pointer) so the sync-orientation class never turns this fixture red."""
    wd = root / name
    (wd / ".coding-hermes" / "board").mkdir(parents=True, exist_ok=True)
    if name.endswith("-sync"):
        base = name[:-len("-sync")]
        (wd / "README.md").write_text(
            f"# {name} — lane workdir\n\nTarget namespace: `{base}`.\n"
            f"Contract: skill `{base}-sync-data`.\n", encoding="utf-8")
    return wd


def _insert_lane(con: sqlite3.Connection, root: Path, name: str, enabled: int,
                 namespace_id: str, cooldown: int) -> None:
    """One project row on the given canonical cadence, with its board dir."""
    wd = _make_workdir(root, name)
    con.execute(
        "INSERT INTO projects (name, enabled, cooldown_s, cooldown_floor_s, command, prompt,"
        " workdir, namespace_id) VALUES (?, ?, ?, ?, ?, '', ?, ?)",
        (name, enabled, cooldown, cooldown, SUPPORTED_COMMAND, str(wd), namespace_id))


def _make_db(path: Path, primary_enabled: int, satellites: list[str],
             disabled_satellites: list[str] | None = None) -> Path:
    """Seed the fixture DB: the ``<base>-foreman`` primary in the
    coding-hermes namespace plus the named ``<base>-<suffix>`` satellites
    (enabled), optionally a further set of satellite rows seeded DISABLED.
    Every lane sits on its canonical family pin and in its family namespace."""
    con = sqlite3.connect(path)
    con.execute("CREATE TABLE projects (name TEXT PRIMARY KEY, enabled INTEGER, cooldown_s INTEGER,"
                " cooldown_floor_s INTEGER, command TEXT, prompt TEXT, workdir TEXT, namespace_id TEXT)")
    con.execute("CREATE TABLE namespaces (id TEXT PRIMARY KEY, max_concurrent INTEGER, admission_mode TEXT)")
    # Empty ticks table: the targets rule queries MAX(spawned_at) on its
    # unresolved-target path, so the table must exist even though this
    # fixture never seeds a tick.
    con.execute("CREATE TABLE ticks (project_name TEXT, spawned_at TEXT, status TEXT)")
    con.execute("INSERT INTO namespaces VALUES ('coding-hermes', 16, 'tasks')")
    for ns in ("qa", "pm", "dogfood", "duckbrain-sync", "releases", "doc-writer"):
        con.execute("INSERT INTO namespaces VALUES (?, ?, 'cooldown')",
                    (ns, gate.SATELLITE_CAP_POLICY.get(ns, 1)))

    _insert_lane(con, path.parent, PRIMARY, primary_enabled, "coding-hermes", 43200)
    for sat in satellites:
        suffix = sat.rsplit("-", 1)[1]
        _insert_lane(con, path.parent, sat, 1,
                     gate.SATELLITE_FAMILY_NAMESPACES[suffix],
                     gate.SATELLITE_FAMILY_PINS[suffix])
    for sat in (disabled_satellites or []):
        suffix = sat.rsplit("-", 1)[1]
        _insert_lane(con, path.parent, sat, 0,
                     gate.SATELLITE_FAMILY_NAMESPACES[suffix],
                     gate.SATELLITE_FAMILY_PINS[suffix])
    con.commit()
    con.close()
    return path


def _run_gate(tmp_path: Path, primary_enabled: int = 1,
              satellites: list[str] | None = None,
              disabled_satellites: list[str] | None = None) -> tuple[int, str]:
    db = _make_db(tmp_path / "scheduler.db", primary_enabled,
                  SATELLITES if satellites is None else satellites,
                  disabled_satellites)
    buf = io.StringIO()
    with redirect_stdout(buf):
        rc = gate.main([
            "--db", str(db),
            "--toml", str(tmp_path / "no-fleet.toml"),
            "--board", str(tmp_path / "no-board.jsonl"),
        ])
    return rc, buf.getvalue()


def _violations(out: str) -> list[str]:
    return [l for l in out.splitlines() if l.startswith("VIOLATION ")]


# ── AC1: the <base>-foreman primary shape is CLEAN ─────────────────────────

def test_foreman_primary_resolves_family_satellites_clean(tmp_path):
    """A ``<base>-foreman`` primary with all four ``<base>-<suffix>``
    satellites passes with zero violations. THE regression pin: removing the
    ``-foreman`` strip makes the coverage rule hunt ``<PRIMARY>-<suffix>``
    children and the targets rule miss the ``<base>`` target — both turn
    this arm red (proven against the pre-fix gate)."""
    rc, out = _run_gate(tmp_path)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert _violations(out) == [], f"unexpected violations:\n{out}"
    assert "PASS" in out, out


# ── AC2: the satellite target resolves THROUGH the -foreman row ────────────

def test_foreman_satellite_target_resolves_to_foreman_row(tmp_path):
    """No project row named ``<base>`` exists — only ``<base>-foreman``. A
    DISABLED foreman row must fire the DISABLED message, which is only
    reachable when the target RESOLVED to that row; the unresolved path
    prints the "neither a fleet project nor a path on disk" message
    instead, so dropping the strip changes this arm's verdict."""
    rc, out = _run_gate(tmp_path, primary_enabled=0)

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out) == [
        f"VIOLATION targets {sat}: target '{BASE}' is DISABLED — the lane would "
        "burn a clean-machine battery on nothing"
        for sat in SATELLITES
    ], f"expected exactly the four DISABLED-target violations (stdout:\n{out})"


# ── AC3: the strip does not deafen the coverage rule ───────────────────────

def test_foreman_family_missing_satellite_still_fires(tmp_path):
    """Drop the pm satellite from an otherwise clean foreman family: the
    coverage rule must still fire, and it must name the FAMILY-shaped child
    (``<base>-pm``), not the raw-lane child (``<base>-foreman-pm``) —
    otherwise the report would send the operator hunting a lane name the
    convention never creates."""
    rc, out = _run_gate(tmp_path, satellites=[s for s in SATELLITES if s != f"{BASE}-pm"])

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out) == [
        f"VIOLATION coverage {BASE}-pm: missing or disabled — every coding-hermes "
        f"primary needs an enabled pm lane (primary '{PRIMARY}' lives in the "
        "'coding-hermes' namespace)"
    ], f"expected exactly the family-shaped coverage violation (stdout:\n{out})"


def test_foreman_family_mixed_absent_and_disabled_still_violates(tmp_path):
    """The parked exemption is narrow: qa DISABLED + pm ABSENT is not a
    parked family — both suffixes violate, per suffix."""
    rc, out = _run_gate(
        tmp_path,
        satellites=[f"{BASE}-sync", f"{BASE}-dogfood"],
        disabled_satellites=[f"{BASE}-qa"],
    )

    assert rc == 1, f"gate exited {rc}, expected 1 (stdout:\n{out})"
    assert _violations(out) == [
        f"VIOLATION coverage {BASE}-pm: missing or disabled — every coding-hermes "
        f"primary needs an enabled pm lane (primary '{PRIMARY}' lives in the "
        "'coding-hermes' namespace)",
        f"VIOLATION coverage {BASE}-qa: missing or disabled — every coding-hermes "
        f"primary needs an enabled qa lane (primary '{PRIMARY}' lives in the "
        "'coding-hermes' namespace)",
    ], f"expected exactly the absent+disabled coverage violations (stdout:\n{out})"


# ── AC4: a parked family (every satellite disabled) is INFO, not a gap ─────

def test_foreman_family_all_satellites_disabled_is_parked_info(tmp_path):
    """warpfs-foreman / hermes-dagger-foreman shape, 2026-11-08: the primary
    is enabled but ALL FOUR satellites exist and are disabled. That is an
    operator decision (a parked family), not a naming defect — the rule
    reports ONE INFO line under the coverage class and stays at exit 0."""
    rc, out = _run_gate(tmp_path, satellites=[], disabled_satellites=SATELLITES)

    assert rc == 0, f"gate exited {rc}, expected 0 (stdout:\n{out})"
    assert _violations(out) == [], f"unexpected violations:\n{out}"
    assert (f"INFO coverage {PRIMARY}: family parked — primary enabled but all "
            "four satellites (qa, pm, sync, dogfood) exist and are disabled") in out, out


# ── AC5: the stem strip is trailing-only ────────────────────────────────────

def test_foreman_family_base_strips_only_trailing_stem():
    """``foreman_family_base`` strips ONLY a trailing ``-foreman``: a real
    primary (``h3-sdk-go-foreman``) strips to its family base, while a
    satellite whose base merely CONTAINS ``foreman``
    (``h3-sdk-go-foreman-sync``) is untouched — mirroring the multi-token
    handling ``sync_readme_declares_namespace`` already documents."""
    f = gate.foreman_family_base
    assert f("schedgap16996-fixture-foreman") == "schedgap16996-fixture"
    assert f("gitreins-foreman") == "gitreins"
    assert f("coding-hermes-scheduler-foreman") == "coding-hermes-scheduler"
    assert f("h3-sdk-go-foreman") == "h3-sdk-go"
    assert f("h3-sdk-go-foreman-sync") == "h3-sdk-go-foreman-sync"
    assert f("foreman") == "foreman"          # no leading hyphen to strip
    assert f("fixture-primary") == "fixture-primary"  # non-foreman: unchanged
